package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/ca"
	"github.com/haasele/prox/internal/dns"
)

func (d *Daemon) Doctor(ctx context.Context, fix bool) (api.DoctorReport, error) {
	var notes []string
	if fix {
		notes = append(notes, d.fix(ctx)...)
	}

	var checks []api.DoctorCheck
	add := func(name, status, detail string) {
		checks = append(checks, api.DoctorCheck{Name: name, Status: status, Detail: detail})
	}

	add("daemon", "ok", fmt.Sprintf("prox %s running on %s", d.version, d.Cfg.APISocket))

	if err := d.Docker.Ping(ctx); err != nil {
		add("docker", "fail", err.Error())
	} else {
		add("docker", "ok", "daemon reachable at "+d.Cfg.DockerSocket)
	}

	if _, err := d.Docker.InspectNetwork(ctx, d.Proxy.Network); err != nil {
		add("prox network", "fail", "network "+d.Proxy.Network+" is missing: "+err.Error())
	} else {
		gw := d.Proxy.GatewayAddr()
		if gw == "" {
			gw = "unknown gateway"
		}
		add("prox network", "ok", d.Proxy.Network+" ("+gw+")")
	}

	httpStatus, httpDetail := d.portCheck(ctx, d.Cfg.HTTPPort, false)
	add(fmt.Sprintf("port %d", d.Cfg.HTTPPort), httpStatus, httpDetail)
	httpsStatus, httpsDetail := d.portCheck(ctx, d.Cfg.HTTPSPort, true)
	add(fmt.Sprintf("port %d", d.Cfg.HTTPSPort), httpsStatus, httpsDetail)

	caStatus, caDetail := "fail", "local CA unavailable"
	pem, err := d.Router.RootCert(ctx)
	var caResult ca.Result
	if err == nil {
		st := ca.Check(pem)
		trusted := st.System || st.NSS || st.Firefox
		switch {
		case trusted && st.System:
			caStatus, caDetail = "ok", "system trust store contains prox local CA"
			if !st.NSS && !st.Firefox {
				caStatus = "warn"
				caDetail += "; NSS (Chrome/Firefox) not detected"
			}
		case trusted:
			caStatus, caDetail = "warn", "trusted only in NSS/Firefox, not the system store"
		default:
			caResult = ca.InstallSystem(pem)
			caDetail = "CA is not trusted; run prox ca install"
			if len(caResult.Commands) > 0 {
				caDetail += " or: " + strings.Join(caResult.Commands, "; ")
			}
		}
	} else {
		caDetail = err.Error()
	}
	add("CA trusted", caStatus, caDetail)

	if addrs, err := dns.Lookup(ctx, d.DNS.Addr(), "prox.localhost"); err != nil {
		add("DNS resolver", "fail", fmt.Sprintf("query %s: %v", d.DNS.Addr(), err))
	} else if !containsLoopback(addrs) {
		add("DNS resolver", "fail", fmt.Sprintf("prox.localhost -> %v, want 127.0.0.1", addrs))
	} else {
		add("DNS resolver", "ok", fmt.Sprintf("%s answers prox.localhost -> 127.0.0.1", d.DNS.Addr()))
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	sys, sysErr := net.DefaultResolver.LookupHost(lookupCtx, "prox.localhost")
	cancel()
	if sysErr != nil || !containsLoopback(sys) {
		detail := "system DNS does not resolve *.localhost"
		if sysErr != nil {
			detail += ": " + sysErr.Error()
		}
		apply := dns.Apply(d.DNS.Addr(), d.DNS.Domains)
		if len(apply.Commands) > 0 {
			detail += "; run prox doctor --fix or: " + strings.Join(apply.Commands, "; ")
		}
		add("system DNS", "warn", detail)
	} else {
		add("system DNS", "ok", fmt.Sprintf("prox.localhost -> %v", sys))
	}

	enf, _ := d.Reg.GetMeta("enforcement")
	if enf == "on" {
		st := d.Authz.Status()
		if st.Running {
			add("enforcement", "ok", "AuthZ plugin listening on "+st.Socket)
		} else {
			add("enforcement", "fail", "enabled but plugin not running: "+st.Error)
		}
	} else {
		add("enforcement", "ok", "off (optional; prox enforce on)")
	}

	if fix && len(notes) > 0 {
		checks = append([]api.DoctorCheck{{Name: "fix", Status: "ok", Detail: strings.Join(notes, "; ")}}, checks...)
	}

	healthy := true
	for _, c := range checks {
		if c.Status == "fail" {
			healthy = false
		}
	}
	return api.DoctorReport{Healthy: healthy, Checks: checks}, nil
}

func (d *Daemon) fix(ctx context.Context) []string {
	var notes []string
	if err := d.Proxy.EnsureNetwork(ctx); err != nil {
		notes = append(notes, "prox network: "+err.Error())
	} else {
		notes = append(notes, "prox network ensured")
	}
	apply := dns.Apply(d.DNS.Addr(), d.DNS.Domains)
	if apply.Applied {
		notes = append(notes, "split DNS configured via "+apply.Backend)
	} else {
		notes = append(notes, apply.Note)
		notes = append(notes, apply.Commands...)
	}
	pem, err := d.Router.RootCert(ctx)
	if err == nil {
		sys := ca.InstallSystem(pem)
		if sys.Applied {
			notes = append(notes, "system CA installed")
		} else {
			notes = append(notes, sys.Commands...)
		}
		nss := ca.InstallNSS(pem)
		if nss.Applied {
			notes = append(notes, "NSS CA installed")
		} else {
			notes = append(notes, nss.Commands...)
		}
	} else {
		notes = append(notes, "CA: "+err.Error())
	}
	return notes
}

func (d *Daemon) portCheck(ctx context.Context, port int, isTLS bool) (string, string) {
	if port <= 0 {
		return "warn", "disabled"
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	ln, err := net.Listen("tcp", ":"+fmt.Sprint(port))
	if err == nil {
		ln.Close()
		return "fail", "port is free; prox is not listening"
	}
	if isTLS {
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "prox.localhost"})
		if err != nil {
			return "fail", "in use, but not a prox TLS listener: " + err.Error()
		}
		defer conn.Close()
		certs := conn.ConnectionState().PeerCertificates
		if len(certs) > 0 {
			issuer := certs[0].Issuer.CommonName
			if owned, err := d.certOwned(ctx, certs); owned {
				return "ok", "serving TLS for prox.localhost (issuer " + issuer + ")"
			} else if err != nil && strings.Contains(issuer, "prox local CA") {
				return "ok", "serving TLS for prox.localhost (issuer " + issuer + ")"
			}
			return "fail", "in use by another TLS service (issuer " + issuer + ")"
		}
		return "fail", "in use by another TLS service"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return "fail", err.Error()
	}
	req.Host = "prox.localhost"
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "fail", "in use, but not responding: " + err.Error()
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "https://") {
		return "ok", "redirecting to " + loc
	}
	return "fail", fmt.Sprintf("in use by another HTTP service (status %d)", resp.StatusCode)
}

func (d *Daemon) certOwned(ctx context.Context, certs []*x509.Certificate) (bool, error) {
	rootPEM, err := d.Router.RootCert(ctx)
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(rootPEM)
	if block == nil {
		return false, fmt.Errorf("bad root PEM")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
	return err == nil, err
}

func containsLoopback(addrs []string) bool {
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}
