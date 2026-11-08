package dns

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type Server struct {
	Listen  string
	Port    int
	Domains []string

	mu     sync.Mutex
	udp    *dns.Server
	tcp    *dns.Server
	closed bool
}

func New(listen string, port int, extraTLDs []string) *Server {
	if listen == "" {
		listen = "127.0.0.1"
	}
	domains := []string{"localhost"}
	for _, d := range extraTLDs {
		d = strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && d != "localhost" {
			domains = append(domains, d)
		}
	}
	return &Server{Listen: listen, Port: port, Domains: domains}
}

func (s *Server) Addr() string {
	return net.JoinHostPort(s.Listen, fmt.Sprintf("%d", s.Port))
}

func (s *Server) Start() error {
	addr := s.Addr()
	s.mu.Lock()
	s.udp = &dns.Server{Addr: addr, Net: "udp", Handler: dns.HandlerFunc(s.handle)}
	s.tcp = &dns.Server{Addr: addr, Net: "tcp", Handler: dns.HandlerFunc(s.handle)}
	s.mu.Unlock()
	errc := make(chan error, 2)
	go func() {
		if err := s.udp.ListenAndServe(); err != nil && !s.isClosed() {
			errc <- err
		}
	}()
	go func() {
		if err := s.tcp.ListenAndServe(); err != nil && !s.isClosed() {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		return fmt.Errorf("dns listen %s: %w", addr, err)
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	udp, tcp := s.udp, s.tcp
	s.mu.Unlock()
	var first error
	if udp != nil {
		if err := udp.ShutdownContext(ctx); err != nil {
			first = err
		}
	}
	if tcp != nil {
		if err := tcp.ShutdownContext(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	if len(req.Question) == 0 {
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	if !s.Matches(name) {
		m.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(m)
		return
	}
	switch q.Qtype {
	case dns.TypeA:
		rr, _ := dns.NewRR(q.Name + " 60 IN A 127.0.0.1")
		m.Answer = append(m.Answer, rr)
	case dns.TypeAAAA:
		rr, _ := dns.NewRR(q.Name + " 60 IN AAAA ::1")
		m.Answer = append(m.Answer, rr)
	case dns.TypeSOA:
		rr, _ := dns.NewRR(q.Name + " 60 IN SOA prox.localhost hostmaster.prox.localhost 1 60 60 60 60")
		m.Answer = append(m.Answer, rr)
	default:
		// NOERROR with no answers for other types.
	}
	_ = w.WriteMsg(m)
}

// Matches reports whether the resolver owns this name.
func (s *Server) Matches(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range s.Domains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// Lookup queries a specific resolver, bypassing the system configuration.
func Lookup(ctx context.Context, addr, host string) ([]string, error) {
	c := &dns.Client{Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	conn, err := c.DialContext(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, _, err := c.ExchangeWithConnContext(ctx, m, conn)
	if err != nil {
		return nil, err
	}
	if resp.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("dns rcode %s", dns.RcodeToString[resp.Rcode])
	}
	var out []string
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out, nil
}
