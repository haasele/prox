package config

import (
	"strings"
	"testing"
)

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PROX_SYSTEM", "1")
	t.Setenv("PROX_STATE_DIR", "/tmp/prox-test/state")
	t.Setenv("PROX_RUNTIME_DIR", "/tmp/prox-test/run")
	t.Setenv("PROX_HTTP_PORT", "18080")
	t.Setenv("PROX_HTTPS_PORT", "18443")
	t.Setenv("PROX_DNS_PORT", "15335")
	t.Setenv("PROX_DB", "/tmp/prox-test/prox.db")
	t.Setenv("PROX_DOCKER_SOCKET", "/tmp/prox-test/docker.sock")
	t.Setenv("PROX_API_SOCKET", "/tmp/prox-test/api.sock")
	t.Setenv("PROX_REAL_DOCKER_SOCKET", "/var/run/docker.sock")
	t.Setenv("PROX_EXTRA_TLDS", "test, .dev")
	t.Setenv("DOCKER_HOST", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPPort != 18080 || cfg.HTTPSPort != 18443 || cfg.DNSPort != 15335 {
		t.Fatalf("ports: %+v", cfg)
	}
	if cfg.TLSLoopbackPort != 18444 {
		t.Fatalf("tls loopback = %d", cfg.TLSLoopbackPort)
	}
	if cfg.SocketPath == cfg.DockerSocket {
		t.Fatal("proxy and real socket must differ")
	}
	if strings.Join(cfg.ExtraTLDs, ",") != "test,dev" {
		t.Fatalf("extra TLDs = %v", cfg.ExtraTLDs)
	}
	if cfg.HTTPSHostPort("prox.localhost") != "prox.localhost:18443" {
		t.Fatalf("host port = %s", cfg.HTTPSHostPort("prox.localhost"))
	}
}

func TestPortCollisionRejected(t *testing.T) {
	t.Setenv("PROX_HTTP_PORT", "18080")
	t.Setenv("PROX_HTTPS_PORT", "18080")
	t.Setenv("PROX_REAL_DOCKER_SOCKET", "/var/run/docker.sock")
	if _, err := Load(); err == nil {
		t.Fatal("expected port collision error")
	}
}
