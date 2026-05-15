package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/diffservice"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	erofsdiff "github.com/containerd/containerd/v2/plugins/diff/erofs"
	snapshot "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pelletier/go-toml/v2"
	"google.golang.org/grpc"

	erofsgrpc "github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc"
	"github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc/credentials"
	"github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc/daemon"
)

const (
	defaultRootDir        = "/var/lib/containerd-erofs/snapshotter"
	defaultSockAddr       = "/run/containerd-erofs-grpc/containerd-erofs-grpc.sock"
	defaultContainerdAddr = "/run/containerd/containerd.sock"
	defaultDaemonMode     = "eager"
	defaultLazydAddr      = "/run/lazyd/lazyd.sock"
	defaultLogLevel       = "info"
)

type serverConfig struct {
	Snapshotter snapshotterConfig `toml:"snapshotter"`
	Containerd  containerdConfig  `toml:"containerd"`
	Registry    registryConfig    `toml:"registry"`
	Daemon      daemonConfig      `toml:"daemon"`
	Log         logConfig         `toml:"log"`
}

type snapshotterConfig struct {
	Root      string `toml:"root"`
	Address   string `toml:"address"`
	Immutable bool   `toml:"immutable"`
}

type containerdConfig struct {
	Address string `toml:"address"`
}

type registryConfig struct {
	DockerConfig string `toml:"docker_config"`
}

type daemonConfig struct {
	Mode  string      `toml:"mode"`
	Lazyd lazydConfig `toml:"lazyd"`
}

type lazydConfig struct {
	LazydBinary  string `toml:"lazyd_binary"`
	LazydAddress string `toml:"lazyd_address"`
}

type logConfig struct {
	Level string `toml:"level"`
}

type cliFlags struct {
	configPath *string
}

func main() {
	cli := registerFlags(flag.CommandLine)
	flag.Parse()

	cfg := defaultServerConfig()
	if *cli.configPath != "" {
		if err := loadServerConfig(*cli.configPath, &cfg); err != nil {
			fmt.Printf("error: load config: %v\n", err)
			os.Exit(1)
		}
	}
	if err := applyFlagOverrides(flag.CommandLine, &cfg); err != nil {
		fmt.Printf("error: parse flags: %v\n", err)
		os.Exit(1)
	}

	if err := log.SetLevel(cfg.Log.Level); err != nil {
		fmt.Printf("error: set log level: %v\n", err)
		os.Exit(1)
	}
	log.L.WithFields(log.Fields{
		"root":            cfg.Snapshotter.Root,
		"addr":            cfg.Snapshotter.Address,
		"containerd_addr": cfg.Containerd.Address,
		"docker_config":   cfg.Registry.DockerConfig,
		"daemon_mode":     cfg.Daemon.Mode,
		"immutable":       cfg.Snapshotter.Immutable,
		"level":           cfg.Log.Level,
	}).Info("Starting containerd-erofs-grpc")

	if err := serve(cfg); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func registerFlags(fs *flag.FlagSet) cliFlags {
	fs.String("root", defaultRootDir, "EROFS snapshotter root directory")
	fs.String("addr", defaultSockAddr, "Socket path to listen on")
	fs.String("containerd-addr", defaultContainerdAddr, "Address for containerd's GRPC server")
	fs.String("docker-config", "", "Optional Docker config directory or config.json path used for registry credentials")
	fs.String("daemon-mode", defaultDaemonMode, "Daemon implementation to use: eager, lazyd")
	fs.String("lazyd-binary", "", "Path to lazyd binary when -daemon-mode=lazyd")
	fs.String("lazyd-addr", defaultLazydAddr, "Socket path used by lazyd when -daemon-mode=lazyd")
	fs.String("log-level", defaultLogLevel, "Log level: trace, debug, info, warn, error, fatal, panic")
	fs.Bool("immutable", false, "Set IMMUTABLE_FL on EROFS layer blobs (default false for performance)")
	return cliFlags{
		configPath: fs.String("config", "", "Optional TOML config file path"),
	}
}

func defaultServerConfig() serverConfig {
	return serverConfig{
		Snapshotter: snapshotterConfig{
			Root:    defaultRootDir,
			Address: defaultSockAddr,
		},
		Containerd: containerdConfig{
			Address: defaultContainerdAddr,
		},
		Daemon: daemonConfig{
			Mode: defaultDaemonMode,
			Lazyd: lazydConfig{
				LazydAddress: defaultLazydAddr,
			},
		},
		Log: logConfig{
			Level: defaultLogLevel,
		},
	}
}

func loadServerConfig(path string, cfg *serverConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return err
	}
	return nil
}

func applyFlagOverrides(fs *flag.FlagSet, cfg *serverConfig) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err != nil {
			return
		}
		switch f.Name {
		case "root":
			cfg.Snapshotter.Root = f.Value.String()
		case "addr":
			cfg.Snapshotter.Address = f.Value.String()
		case "containerd-addr":
			cfg.Containerd.Address = f.Value.String()
		case "docker-config":
			cfg.Registry.DockerConfig = f.Value.String()
		case "daemon-mode":
			cfg.Daemon.Mode = f.Value.String()
		case "lazyd-binary":
			cfg.Daemon.Lazyd.LazydBinary = f.Value.String()
		case "lazyd-addr":
			cfg.Daemon.Lazyd.LazydAddress = f.Value.String()
		case "log-level":
			cfg.Log.Level = f.Value.String()
		case "immutable":
			cfg.Snapshotter.Immutable, err = strconv.ParseBool(f.Value.String())
		case "config":
		}
	})
	return err
}

func serve(cfg serverConfig) error {
	containerdAddress := cfg.Containerd.Address
	address := cfg.Snapshotter.Address
	root := cfg.Snapshotter.Root

	// Prepare the address directory
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		return err
	}
	// Remove the socket if exist to avoid EADDRINUSE
	if err := os.RemoveAll(address); err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(streamServerInterceptor),
		grpc.UnaryInterceptor(unaryServerInterceptor),
	}

	rpc := grpc.NewServer(serverOpts...)

	client, err := containerd.New(containerdAddress)
	if err != nil {
		return err
	}
	defer client.Close()

	// Instantiate the EROFS differ
	d := &diffService{contentStore: client.ContentStore()}
	service := diffservice.FromApplierAndComparer(d, d)
	diffapi.RegisterDiffServer(rpc, service)

	var opts []snapshot.Opt
	if cfg.Snapshotter.Immutable {
		opts = append(opts, snapshot.WithImmutable())
	}
	baseSnapshotter, err := snapshot.NewSnapshotter(root, opts...)
	if err != nil {
		return err
	}

	creds := credentials.NewDockerConfigBackend(cfg.Registry.DockerConfig)
	daemonClient, err := newDaemonClient(cfg.Daemon)
	if err != nil {
		return err
	}
	erofsGRPCSnapshotter, err := erofsgrpc.New(erofsgrpc.Config{
		Root: root,
		Base: baseSnapshotter,
		ManifestProvider: erofsgrpc.NewManifestProvider(erofsgrpc.ManifestProviderConfig{
			ContentStore: client.ContentStore(),
			Credentials:  creds,
		}),
		BlobProvider: erofsgrpc.NewBlobProvider(erofsgrpc.BlobProviderConfig{
			ContentStore: client.ContentStore(),
			Credentials:  creds,
		}),
		Daemon:       daemonClient,
		DaemonConfig: erofsgrpc.DaemonConfig{Root: root},
	})
	if err != nil {
		return err
	}
	defer erofsGRPCSnapshotter.Close()

	// Convert the snapshotter to a gRPC service,
	// example in github.com/containerd/containerd/contrib/snapshotservice
	ss := snapshotservice.FromSnapshotter(erofsGRPCSnapshotter)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, ss)

	// Listen and serve
	l, err := net.Listen("unix", address)
	if err != nil {
		return err
	}
	log.L.WithFields(log.Fields{
		"listen_addr":     address,
		"root":            root,
		"containerd_addr": containerdAddress,
	}).Info("Listening")
	return rpc.Serve(l)
}

func newDaemonClient(cfg daemonConfig) (daemon.DaemonClient, error) {
	switch cfg.Mode {
	case "eager":
		return daemon.NewEagerDaemon(), nil
	case "lazyd":
		return daemon.NewLazyDaemon(daemon.LazyDaemonConfig{
			Binary: cfg.Lazyd.LazydBinary,
			Socket: cfg.Lazyd.LazydAddress,
		})
	default:
		return nil, fmt.Errorf("unsupported daemon mode %q", cfg.Mode)
	}
}

func unaryServerInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
	}
	return handler(ctx, req)
}

func streamServerInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
		ss = &wrappedSSWithContext{ctx: ctx, ServerStream: ss}
	}
	return handler(srv, ss)
}

type wrappedSSWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedSSWithContext) Context() context.Context {
	return w.ctx
}

type differ interface {
	diff.Applier
	diff.Comparer
}

type diffService struct {
	contentStore content.Store
	differ       differ
	loaded       uint32
	loadM        sync.Mutex

	diffapi.UnimplementedDiffServer
}

func (a *diffService) getDiffer() (differ, error) {
	if atomic.LoadUint32(&a.loaded) == 1 {
		return a.differ, nil
	}

	a.loadM.Lock()
	defer a.loadM.Unlock()

	if a.loaded == 1 {
		return a.differ, nil
	}

	if a.contentStore == nil {
		return nil, errors.New("content store is not configured")
	}

	a.differ = erofsdiff.NewErofsDiffer(a.contentStore, []string{})
	atomic.StoreUint32(&a.loaded, 1)
	return a.differ, nil
}

func (s *diffService) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Apply(ctx, desc, mounts, opts...)
}

func (s *diffService) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Compare(ctx, lower, upper, opts...)
}
