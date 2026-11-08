package dockerproxy

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"

	"github.com/haasele/prox/internal/registry"
)

func testAllocator(t *testing.T, start int) (*Allocator, *registry.Registry) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "alloc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return NewAllocator(reg, start, start+20, func(p int) bool { return p > 1024 }, slog.New(slog.NewTextHandler(io.Discard, nil))), reg
}

func occupy(t *testing.T, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", ":"+itoa(port))
	if err != nil {
		t.Skipf("cannot occupy port %d: %v", port, err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [6]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestAllocatorPreferredAndFallback(t *testing.T) {
	start := 29000
	for {
		ln, err := net.Listen("tcp", ":"+itoa(start))
		if err == nil {
			ln.Close()
			break
		}
		start++
	}
	a, _ := testAllocator(t, start)
	got, err := a.Allocate(0, "k1")
	if err != nil || got != start {
		t.Fatalf("got %d err %v, want %d", got, err, start)
	}
	// Same key is idempotent.
	if got2, err := a.Allocate(0, "k1"); err != nil || got2 != got {
		t.Fatalf("idempotent: %d %v", got2, err)
	}
	// Different key skips the reservation.
	got2, err := a.Allocate(0, "k2")
	if err != nil || got2 == got {
		t.Fatalf("second allocation: %d %v", got2, err)
	}
}

func TestAllocatorExplicitBusy(t *testing.T) {
	// Pick a free range and occupy its preferred port.
	start := 30000
	for {
		ln, err := net.Listen("tcp", ":"+itoa(start))
		if err == nil {
			ln.Close()
			break
		}
		start++
	}
	a, _ := testAllocator(t, start+2)
	occupy(t, start)
	if _, err := a.AllocateExplicit(start, "explicit"); err == nil {
		t.Fatal("explicit busy port should error")
	}
	// Implicit falls through into the range.
	got, err := a.Allocate(start, "implicit")
	if err != nil {
		t.Fatal(err)
	}
	if got == start {
		t.Fatalf("implicit allocation reused busy port")
	}
	// Explicit free port works.
	free := start + 1
	if !portFree(free) {
		t.Skipf("port %d unexpectedly busy", free)
	}
	got, err = a.AllocateExplicit(free, "explicit2")
	if err != nil || got != free {
		t.Fatalf("explicit free: %d %v", got, err)
	}
}
