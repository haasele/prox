package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all daemon and client settings. Every field can be overridden
// with a PROX_* environment variable, which keeps e2e tests off privileged
// ports and away from a user's real state.
type Config struct {
	System       bool
	StateDir     string
	RuntimeDir   string
	DBPath       string
	SocketPath   string // Docker API proxy socket
	APISocket    string
	AuthzSocket  string
	CAStorageDir string
	RulesFile    string
	ProfilesDir  string
	WebDir       string

	HTTPPort        int
	HTTPSPort       int
	HTTPSListen     string
	TLSLoopbackPort int
	DNSPort         int
	DNSListen       string
	ExtraTLDs       []string

	GatewayPortStart int
	GatewayPortEnd   int
	DockerSocket     string
	Backend          string
	Enforcement      bool
	LogLevel         string
	ProjectDomain    string
	SocketGroup      string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// Load builds the effective configuration. When running as root (system
// daemon) it defaults to /var/lib/prox and /run/prox; otherwise it uses XDG
// directories. PROX_SYSTEM=1 forces system paths.
func Load() (*Config, error) {
	u, _ := user.Current()
	uid := 0
	if u != nil {
		uid, _ = strconv.Atoi(u.Uid)
	}
	system := envBool("PROX_SYSTEM", uid == 0)

	home := os.Getenv("HOME")
	if home == "" {
		if u != nil {
			home = u.HomeDir
		}
	}
	stateBase := env("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	configBase := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	runtimeBase := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeBase == "" {
		runtimeBase = filepath.Join(stateBase, "prox")
	}

	// A non-root CLI should find a system-wide install without requiring
	// PROX_SYSTEM=1 in every shell. Prefer a user daemon when one is running;
	// otherwise fall back to the system sockets when /etc/proxd or the system
	// API socket exists. Explicit path/env overrides always win.
	if os.Getenv("PROX_SYSTEM") == "" && uid != 0 &&
		os.Getenv("PROX_STATE_DIR") == "" && os.Getenv("PROX_RUNTIME_DIR") == "" &&
		os.Getenv("PROX_API_SOCKET") == "" {
		_, sysSock := os.Stat("/run/prox/api.sock")
		_, sysMarker := os.Stat("/etc/proxd")
		// A system install wins for non-root clients. Set PROX_SYSTEM=0 or an
		// explicit PROX_RUNTIME_DIR/PROX_API_SOCKET to use a user daemon.
		if sysSock == nil || sysMarker == nil {
			system = true
		}
	}

	cfg := &Config{
		System:           system,
		LogLevel:         env("PROX_LOG_LEVEL", "info"),
		HTTPPort:         envInt("PROX_HTTP_PORT", 80),
		HTTPSPort:        envInt("PROX_HTTPS_PORT", 443),
		HTTPSListen:      env("PROX_HTTPS_LISTEN", ""),
		TLSLoopbackPort:  envInt("PROX_TLS_LOOPBACK_PORT", 0),
		DNSPort:          envInt("PROX_DNS_PORT", 5335),
		DNSListen:        env("PROX_DNS_LISTEN", "127.0.0.1"),
		ExtraTLDs:        splitCSV(env("PROX_EXTRA_TLDS", "")),
		GatewayPortStart: envInt("PROX_GATEWAY_PORT_START", 20000),
		GatewayPortEnd:   envInt("PROX_GATEWAY_PORT_END", 29999),
		Backend:          env("PROX_BACKEND", "auto"),
		Enforcement:      envBool("PROX_ENFORCE", false),
		ProjectDomain:    env("PROX_DOMAIN", "localhost"),
		SocketGroup:      env("PROX_SOCKET_GROUP", map[bool]string{true: "docker", false: ""}[system]),
	}

	if cfg.HTTPPort == cfg.HTTPSPort {
		return nil, fmt.Errorf("PROX_HTTP_PORT and PROX_HTTPS_PORT must differ")
	}
	if cfg.TLSLoopbackPort == 0 {
		cfg.TLSLoopbackPort = cfg.HTTPSPort + 1
		if cfg.TLSLoopbackPort == cfg.HTTPPort {
			cfg.TLSLoopbackPort++
		}
	}
	if cfg.GatewayPortStart <= cfg.HTTPSPort {
		cfg.GatewayPortStart = cfg.HTTPSPort + 1000
	}
	if cfg.GatewayPortEnd < cfg.GatewayPortStart {
		cfg.GatewayPortEnd = cfg.GatewayPortStart + 9999
	}

	if system {
		cfg.StateDir = env("PROX_STATE_DIR", "/var/lib/prox")
		cfg.RuntimeDir = env("PROX_RUNTIME_DIR", "/run/prox")
		cfg.RulesFile = env("PROX_RULES_FILE", "/etc/proxd/rules.yaml")
		cfg.ProfilesDir = env("PROX_PROFILES_DIR", "/etc/prox/profiles.d")
	} else {
		cfg.StateDir = env("PROX_STATE_DIR", filepath.Join(stateBase, "prox"))
		cfg.RuntimeDir = env("PROX_RUNTIME_DIR", filepath.Join(runtimeBase, "prox"))
		cfg.RulesFile = env("PROX_RULES_FILE", filepath.Join(configBase, "prox", "rules.yaml"))
		cfg.ProfilesDir = env("PROX_PROFILES_DIR", filepath.Join(configBase, "prox", "profiles.d"))
	}

	cfg.DBPath = env("PROX_DB", filepath.Join(cfg.StateDir, "prox.db"))
	cfg.SocketPath = env("PROX_DOCKER_SOCKET", filepath.Join(cfg.RuntimeDir, "docker.sock"))
	cfg.APISocket = env("PROX_API_SOCKET", filepath.Join(cfg.RuntimeDir, "api.sock"))
	cfg.AuthzSocket = env("PROX_AUTHZ_SOCKET", "/run/docker/plugins/prox-authz.sock")
	cfg.CAStorageDir = env("PROX_CA_DIR", filepath.Join(cfg.StateDir, "ca"))
	cfg.WebDir = env("PROX_WEB_DIR", "")

	cfg.DockerSocket = env("PROX_REAL_DOCKER_SOCKET", "")
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = detectDockerSocket(cfg.SocketPath)
	}
	if cfg.SocketPath == cfg.DockerSocket {
		return nil, fmt.Errorf("proxy socket and real Docker socket are the same path: %s", cfg.SocketPath)
	}
	return cfg, nil
}

func detectDockerSocket(proxySocket string) string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		p := strings.TrimPrefix(h, "unix://")
		// If DOCKER_HOST already points at our own proxy (e.g. a daemon
		// restarted inside a shell that exports it) fall back to the real
		// daemon instead of looping.
		if p != proxySocket && !strings.HasSuffix(p, "/prox/docker.sock") {
			return p
		}
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		p := filepath.Join(xdg, "docker.sock")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if u, err := user.Current(); err == nil && u.Uid != "0" {
		p := filepath.Join("/run/user", u.Uid, "docker.sock")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/var/run/docker.sock"
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimPrefix(strings.TrimSpace(p), ".")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnsureDirs creates the state and runtime directories with restrictive
// permissions. The runtime dir is 0755 so the docker group can reach the
// proxy socket; the sockets themselves are tightened after listen.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.StateDir, c.RuntimeDir, c.CAStorageDir, filepath.Dir(c.RulesFile), c.ProfilesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if !c.System {
		_ = os.Chmod(c.StateDir, 0o700)
		_ = os.Chmod(c.CAStorageDir, 0o700)
	}
	return nil
}

func (c *Config) HTTPScheme() string {
	return "https"
}

func (c *Config) HTTPSHostPort(host string) string {
	if c.HTTPSPort == 443 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, c.HTTPSPort)
}

func (c *Config) HTTPHostPort(host string) string {
	if c.HTTPPort == 80 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, c.HTTPPort)
}

// ShareSocketWithGroup grants the configured group (docker by default) access
// to a socket. Users in the docker group already control the daemon, so the
// prox API does not widen their effective privileges.
func (c *Config) ShareSocketWithGroup(path string) {
	if c.SocketGroup == "" {
		return
	}
	g, err := user.LookupGroup(c.SocketGroup)
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return
	}
	_ = os.Chown(path, -1, gid)
}
