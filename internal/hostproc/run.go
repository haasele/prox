package hostproc

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// FreePort reserves and releases an ephemeral loopback port, returning it for
// the child process to bind.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	err = ln.Close()
	return port, err
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// WatchPID blocks until the process exits or ctx is done. It uses pidfd where
// the kernel supports it and falls back to polling.
func WatchPID(ctx context.Context, pid int) {
	if pid <= 0 {
		return
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		defer unix.Close(fd)
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(pfd, 1000)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return
			}
			if n > 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !Alive(pid) {
				return
			}
		}
	}
}

func CommandString(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"$`\\*?[]{}();&|<>") {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
	}
	return strings.Join(parts, " ")
}

// Environment merges the current environment with overrides, last wins.
func Environment(overrides []string) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func PortFromEnv(env []string) int {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PORT=") {
			p, _ := strconv.Atoi(strings.TrimPrefix(kv, "PORT="))
			return p
		}
	}
	return 0
}
