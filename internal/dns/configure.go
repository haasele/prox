package dns

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ApplyResult struct {
	Backend  string
	Applied  bool
	Commands []string
	Note     string
}

// Detect returns the active local DNS configuration backend.
func Detect() string {
	if exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run() == nil {
		return "systemd-resolved"
	}
	if exec.Command("systemctl", "is-active", "--quiet", "NetworkManager").Run() == nil {
		return "networkmanager"
	}
	if _, err := os.Stat("/etc/NetworkManager"); err == nil {
		return "networkmanager"
	}
	return "none"
}

// Apply configures split DNS for prox names. When not running as root it
// returns the exact commands to run instead of failing.
func Apply(addr string, domains []string) ApplyResult {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "53"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	backend := Detect()
	res := ApplyResult{Backend: backend}
	var domainList []string
	for _, d := range domains {
		domainList = append(domainList, "~"+strings.TrimPrefix(d, "."))
	}
	domainsField := strings.Join(domainList, " ")

	switch backend {
	case "systemd-resolved":
		content := fmt.Sprintf("[Resolve]\nDNS=%s:%s\nDomains=%s\n", host, port, domainsField)
		path := "/etc/systemd/resolved.conf.d/prox.conf"
		res.Commands = []string{
			"sudo mkdir -p /etc/systemd/resolved.conf.d",
			fmt.Sprintf("printf %s | sudo tee %s", shellQuote(content), path),
			"sudo resolvectl flush-caches",
		}
		res.Note = "split DNS for " + domainsField + " via systemd-resolved"
		if os.Geteuid() == 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err := os.WriteFile(path, []byte(content), 0o644); err == nil {
					_ = exec.Command("resolvectl", "flush-caches").Run()
					res.Applied = true
				}
			}
		}
	case "networkmanager":
		var lines []string
		for _, d := range domains {
			lines = append(lines, fmt.Sprintf("server=/%s/%s#%s", d, host, port))
		}
		content := strings.Join(lines, "\n") + "\n"
		path := "/etc/NetworkManager/dnsmasq.d/prox.conf"
		res.Commands = []string{
			"sudo mkdir -p /etc/NetworkManager/dnsmasq.d",
			fmt.Sprintf("printf %s | sudo tee %s", shellQuote(content), path),
			"sudo systemctl reload NetworkManager",
		}
		res.Note = "split DNS via NetworkManager dnsmasq"
		if os.Geteuid() == 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err := os.WriteFile(path, []byte(content), 0o644); err == nil {
					_ = exec.Command("systemctl", "reload", "NetworkManager").Run()
					res.Applied = true
				}
			}
		}
	default:
		res.Note = "no supported DNS manager detected; add these names to /etc/hosts or use gw doctor --fix on a systemd-resolved machine"
		res.Commands = []string{
			fmt.Sprintf("echo '127.0.0.1 gw.localhost' | sudo tee -a /etc/hosts  # fallback only"),
		}
	}
	return res
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

const (
	hostsBegin = "# BEGIN prox managed block"
	hostsEnd   = "# END prox managed block"
)

// WriteHostsFallback maintains an optional /etc/hosts block. It is off by
// default and only used when the administrator opts in.
func WriteHostsFallback(path string, hosts []string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(content), "\n")
	var out []string
	skip := false
	for _, line := range lines {
		if strings.TrimSpace(line) == hostsBegin {
			skip = true
			continue
		}
		if strings.TrimSpace(line) == hostsEnd {
			skip = false
			continue
		}
		if !skip {
			out = append(out, line)
		}
	}
	out = append(out, hostsBegin)
	for _, h := range hosts {
		out = append(out, "127.0.0.1 "+h)
	}
	out = append(out, hostsEnd, "")
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}
