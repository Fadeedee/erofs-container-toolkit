package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServerConfig(t *testing.T) {
	path := writeConfig(t, `
[snapshotter]
root = "/tmp/erofs-root"
address = "/tmp/containerd-erofs-grpc.sock"
immutable = true

[containerd]
address = "/tmp/containerd.sock"

[registry]
docker_config = "/tmp/docker-config"

[daemon]
mode = "lazyd"

[daemon.lazyd]
lazyd_binary = "/usr/bin/lazyd"
lazyd_address = "/tmp/lazyd.sock"

[log]
level = "debug"
`)

	cfg := defaultServerConfig()
	if err := loadServerConfig(path, &cfg); err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Snapshotter.Root != "/tmp/erofs-root" {
		t.Fatalf("snapshotter root = %q", cfg.Snapshotter.Root)
	}
	if cfg.Snapshotter.Address != "/tmp/containerd-erofs-grpc.sock" {
		t.Fatalf("snapshotter address = %q", cfg.Snapshotter.Address)
	}
	if !cfg.Snapshotter.Immutable {
		t.Fatal("snapshotter immutable = false")
	}
	if cfg.Containerd.Address != "/tmp/containerd.sock" {
		t.Fatalf("containerd address = %q", cfg.Containerd.Address)
	}
	if cfg.Registry.DockerConfig != "/tmp/docker-config" {
		t.Fatalf("docker config = %q", cfg.Registry.DockerConfig)
	}
	if cfg.Daemon.Mode != "lazyd" {
		t.Fatalf("daemon mode = %q", cfg.Daemon.Mode)
	}
	if cfg.Daemon.Lazyd.LazydBinary != "/usr/bin/lazyd" {
		t.Fatalf("lazyd binary = %q", cfg.Daemon.Lazyd.LazydBinary)
	}
	if cfg.Daemon.Lazyd.LazydAddress != "/tmp/lazyd.sock" {
		t.Fatalf("lazyd address = %q", cfg.Daemon.Lazyd.LazydAddress)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log level = %q", cfg.Log.Level)
	}
}

func TestExplicitFlagsOverrideConfig(t *testing.T) {
	path := writeConfig(t, `
[snapshotter]
root = "/config/root"
address = "/config/containerd-erofs-grpc.sock"
immutable = true

[containerd]
address = "/config/containerd.sock"

[daemon]
mode = "lazyd"

[daemon.lazyd]
lazyd_binary = "/config/lazyd"
lazyd_address = "/config/lazyd.sock"

[log]
level = "debug"
`)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cli := registerFlags(fs)
	if err := fs.Parse([]string{
		"--config", path,
		"--root", "/flag/root",
		"--daemon-mode", "eager",
		"--immutable=false",
		"--log-level", "warn",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	cfg := defaultServerConfig()
	if *cli.configPath != "" {
		if err := loadServerConfig(*cli.configPath, &cfg); err != nil {
			t.Fatalf("load config: %v", err)
		}
	}
	if err := applyFlagOverrides(fs, &cfg); err != nil {
		t.Fatalf("apply overrides: %v", err)
	}

	if cfg.Snapshotter.Root != "/flag/root" {
		t.Fatalf("snapshotter root = %q", cfg.Snapshotter.Root)
	}
	if cfg.Snapshotter.Address != "/config/containerd-erofs-grpc.sock" {
		t.Fatalf("snapshotter address = %q", cfg.Snapshotter.Address)
	}
	if cfg.Snapshotter.Immutable {
		t.Fatal("snapshotter immutable = true")
	}
	if cfg.Containerd.Address != "/config/containerd.sock" {
		t.Fatalf("containerd address = %q", cfg.Containerd.Address)
	}
	if cfg.Daemon.Mode != "eager" {
		t.Fatalf("daemon mode = %q", cfg.Daemon.Mode)
	}
	if cfg.Daemon.Lazyd.LazydBinary != "/config/lazyd" {
		t.Fatalf("lazyd binary = %q", cfg.Daemon.Lazyd.LazydBinary)
	}
	if cfg.Log.Level != "warn" {
		t.Fatalf("log level = %q", cfg.Log.Level)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "containerd-erofs-grpc.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
