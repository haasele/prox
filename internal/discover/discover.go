// Package discover finds HTTP dev servers already listening on loopback and
// returns enough context for prox to give them a hostname. It is Linux-only;
// other systems return no listeners.
package discover

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/haasele/prox/internal/policy"
)

// Listener is an HTTP server running on a loopback port.
type Listener struct {
	Port       int
	PID        int
	Name       string
	Dir        string
	Exec       string
	Command    string
	StartTicks uint64
}

type socket struct {
	port  int
	inode uint64
	ipv6  bool
}

// Scan returns HTTP listeners on loopback ports >= 1024 whose process cwd is
// outside system directories. excludePID skips the caller's own listeners.
func Scan(excludePID int) []Listener {
	return ScanSkip(excludePID, nil)
}

// ScanSkip is Scan with a set of ports to ignore before probing them.
func ScanSkip(excludePID int, skip map[int]bool) []Listener {
	if runtime.GOOS != "linux" {
		return nil
	}
	socks := map[uint64]socket{}
	seenPort := map[int]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		for _, s := range parseProcNet(path) {
			if s.port < 1024 || seenPort[s.port] || skip[s.port] {
				continue
			}
			seenPort[s.port] = true
			socks[s.inode] = s
		}
	}
	if len(socks) == 0 {
		return nil
	}

	pids := mapSockets(socks)
	var out []Listener
	for inode, s := range socks {
		pid, ok := pids[inode]
		if !ok || pid == excludePID {
			continue
		}
		dir, comm, exe, cmd, startTicks := processInfo(pid)
		if dir == "" || systemDir(dir) {
			continue
		}
		if s.port > 19999 {
			continue
		}
		if !allowedRuntime(exe, comm) && !hasManifest(dir) {
			continue
		}
		if deniedRuntime(exe, comm) {
			continue
		}
		if !probeHTTP(s) {
			continue
		}
		out = append(out, Listener{
			Port:       s.port,
			PID:        pid,
			Name:       deriveName(dir, comm),
			Dir:        dir,
			Exec:       filepath.Base(exe),
			Command:    cmd,
			StartTicks: startTicks,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// parseProcNet reads a /proc/net/tcp{,6} table and returns LISTEN sockets bound
// to loopback or all interfaces.
func parseProcNet(path string) []socket {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []socket
	ipv6 := strings.HasSuffix(path, "6")
	for _, line := range strings.Split(string(b), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		addrPort := strings.Split(fields[1], ":")
		if len(addrPort) != 2 {
			continue
		}
		port64, err := strconv.ParseUint(addrPort[1], 16, 16)
		if err != nil {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		addr := strings.ToUpper(addrPort[0])
		switch {
		case !ipv6 && (addr == "00000000" || addr == "0100007F"):
		case ipv6 && (addr == strings.Repeat("0", 32) || addr == "00000000000000000000000001000000"):
		default:
			continue
		}
		out = append(out, socket{port: int(port64), inode: inode, ipv6: ipv6})
	}
	return out
}

// mapSockets maps a socket inode to the PID owning it.
func mapSockets(socks map[uint64]socket) map[uint64]int {
	pids := map[uint64]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return pids
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			if _, ok := socks[inode]; ok {
				pids[inode] = pid
			}
		}
	}
	return pids
}

func processInfo(pid int) (dir, comm, exe, cmd string, startTicks uint64) {
	dir, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	exe, _ = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		cmd = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		startTicks = statStartTicks(string(b))
	}
	return dir, comm, exe, cmd, startTicks
}

// statStartTicks extracts field 22 (starttime) from /proc/<pid>/stat. The comm
// field may contain spaces and parentheses, so parsing starts after the last
// closing parenthesis.
func statStartTicks(stat string) uint64 {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0
	}
	fields := strings.Fields(stat[i+2:])
	// fields[0] is field 3 (state); field 22 is at index 19.
	if len(fields) <= 19 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[19], 10, 64)
	return v
}

// SelfStartTicks returns this process's starttime in clock ticks since boot.
func SelfStartTicks() uint64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	return statStartTicks(string(b))
}

func probeHTTP(s socket) bool {
	hosts := []string{"127.0.0.1"}
	if s.ipv6 {
		hosts = append([]string{"::1"}, hosts...)
	}
	client := &http.Client{
		Timeout: 400 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, host := range hosts {
		req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(s.port))+"/", nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "prox-discovery")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		return true
	}
	return false
}

func deriveName(dir, comm string) string {
	if dir != "" && dir != "/" {
		if base := filepath.Base(dir); base != "." && base != "/" && base != "" {
			return policy.SanitizeLabel(base)
		}
	}
	return policy.SanitizeLabel(comm)
}

var runtimes = []string{
	"node", "bun", "bunx", "deno", "npm", "pnpm", "yarn",
	"python", "uvicorn", "gunicorn", "hypercorn", "flask", "daphne",
	"go", "air", "ruby", "rails", "puma", "rackup",
	"php", "artisan", "mix", "beam.smp", "java", "dotnet",
}

func allowedRuntime(exe, comm string) bool {
	for _, name := range []string{filepath.Base(exe), comm} {
		name = strings.ToLower(name)
		for _, runtime := range runtimes {
			if name == runtime {
				return true
			}
		}
		if strings.HasPrefix(name, "python") {
			return true
		}
	}
	return false
}

func deniedRuntime(exe, comm string) bool {
	for _, name := range []string{filepath.Base(exe), comm} {
		switch strings.ToLower(name) {
		case "docker-proxy", "containerd", "dockerd", "prox", "proxd", "caddy":
			return true
		}
	}
	return false
}

func hasManifest(dir string) bool {
	for _, name := range []string{
		"package.json", "pyproject.toml", "requirements.txt", "uv.lock",
		"go.mod", "Cargo.toml", "Gemfile", "composer.json", "deno.json",
		"mix.exs", "pom.xml", "build.gradle",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func systemDir(dir string) bool {
	if dir == "" || dir == "/" {
		return true
	}
	for _, prefix := range []string{"/usr", "/var", "/etc", "/run", "/boot", "/nix", "/snap", "/proc", "/sys", "/opt"} {
		if dir == prefix || strings.HasPrefix(dir, prefix+"/") {
			return true
		}
	}
	return false
}
