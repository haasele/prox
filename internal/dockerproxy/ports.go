package dockerproxy

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/haasele/prox/internal/registry"
)

// Allocator hands out dedicated gateway TCP ports. A port is never reassigned
// while its reservation key is active; reservations are persisted in SQLite so
// a daemon restart keeps them.
type Allocator struct {
	Reg     *registry.Registry
	Start   int
	End     int
	CanBind func(int) bool
	Log     *slog.Logger
}

func NewAllocator(reg *registry.Registry, start, end int, canBind func(int) bool, log *slog.Logger) *Allocator {
	if start <= 0 {
		start = 20000
	}
	if end < start {
		end = start + 9999
	}
	return &Allocator{Reg: reg, Start: start, End: end, CanBind: canBind, Log: log}
}

func (a *Allocator) Allocate(preferred int, key string) (int, error) {
	return a.allocate(preferred, key, false)
}

// AllocateExplicit fails instead of falling through when the user named a
// gateway port and it is unavailable.
func (a *Allocator) AllocateExplicit(preferred int, key string) (int, error) {
	return a.allocate(preferred, key, true)
}

func (a *Allocator) allocate(preferred int, key string, explicit bool) (int, error) {
	if existing, ok := a.Reg.GatewayPortFor(key); ok {
		return existing, nil
	}
	reserved, err := a.Reg.ListGatewayPorts()
	if err != nil {
		return 0, err
	}
	usable := func(p int) bool {
		if p <= 0 || p > 65535 || p == 443 {
			return false
		}
		if a.CanBind != nil && !a.CanBind(p) {
			return false
		}
		if _, taken := reserved[p]; taken {
			return false
		}
		return portFree(p)
	}
	if preferred > 0 {
		if owner, ok := reserved[preferred]; ok && owner == key {
			return preferred, nil
		}
		if usable(preferred) {
			if err := a.Reg.ReserveGatewayPort(preferred, key); err != nil {
				return 0, err
			}
			return preferred, nil
		}
		if explicit {
			return 0, fmt.Errorf("gateway port %d is already in use; choose another with --target/--gateway-port", preferred)
		}
	}
	for p := a.Start; p <= a.End; p++ {
		if _, taken := reserved[p]; taken {
			continue
		}
		if !usable(p) {
			continue
		}
		if err := a.Reg.ReserveGatewayPort(p, key); err != nil {
			// Another create raced us; keep scanning.
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free gateway port in range %d-%d", a.Start, a.End)
}

func (a *Allocator) Release(key string) {
	a.Reg.ReleaseGatewayPortByKey(key)
}

// CheckPort reports whether a port can be bound for a new gateway listener
// without mutating reservations.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
