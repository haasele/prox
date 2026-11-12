package dns

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestResolver(t *testing.T) {
	port := freeUDPPort(t)
	s := New("127.0.0.1", port, []string{"test"})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	c := &dns.Client{Timeout: 2 * time.Second}
	for _, host := range []string{"prox.localhost", "web.shop.localhost", "api.test"} {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), dns.TypeA)
		resp, _, err := c.Exchange(m, s.Addr())
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
			t.Fatalf("%s: rcode=%d answers=%d", host, resp.Rcode, len(resp.Answer))
		}
		if a, ok := resp.Answer[0].(*dns.A); !ok || a.A.String() != "127.0.0.1" {
			t.Fatalf("%s: answer %v", host, resp.Answer[0])
		}
	}

	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	resp, _, err := c.Exchange(m, s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("external rcode = %d", resp.Rcode)
	}

	m = new(dns.Msg)
	m.SetQuestion("prox.localhost.", dns.TypeAAAA)
	resp, _, err = c.Exchange(m, s.Addr())
	if err != nil || len(resp.Answer) != 1 {
		t.Fatalf("AAAA: %v %+v", err, resp)
	}

	// TCP as well.
	tc := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	m = new(dns.Msg)
	m.SetQuestion("prox.localhost.", dns.TypeA)
	if _, _, err := tc.Exchange(m, s.Addr()); err != nil {
		t.Fatalf("tcp: %v", err)
	}

	addrs, err := Lookup(context.Background(), s.Addr(), "prox.localhost")
	if err != nil || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Fatalf("Lookup = %v, %v", addrs, err)
	}
}

func TestMatches(t *testing.T) {
	s := New("127.0.0.1", 5353, nil)
	if !s.Matches("foo.localhost") || !s.Matches("localhost") {
		t.Fatal("localhost should match")
	}
	if s.Matches("foo.test") || s.Matches("localhost.evil.com") {
		t.Fatal("unexpected match")
	}
}

func TestHostsFallback(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hosts"
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostsFallback(path, []string{"prox.localhost", "app.localhost"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostsFallback(path, []string{"prox.localhost", "app.localhost"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Count(s, hostsBegin) != 1 || strings.Count(s, hostsEnd) != 1 {
		t.Fatalf("managed block duplicated:\n%s", s)
	}
	if !strings.Contains(s, "127.0.0.1 app.localhost") || !strings.Contains(s, "127.0.0.1 localhost") {
		t.Fatalf("hosts content:\n%s", s)
	}
}
