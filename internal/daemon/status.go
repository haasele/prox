package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/haasele/prox/internal/api"
)

func (d *Daemon) caRootPath() string {
	return filepath.Join(d.Cfg.CAStorageDir, "pki", "authorities", "local", "root.crt")
}

func (d *Daemon) StatusExtra(ctx context.Context) api.Status {
	st := api.Status{
		Backend: d.Backend.Name(),
		Network: d.Proxy.Network,
		Gateway: d.Proxy.GatewayAddr(),
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := d.Docker.Ping(c); err != nil {
		st.DockerOK = false
		st.DockerError = err.Error()
	} else {
		st.DockerOK = true
	}
	if v, _ := d.Reg.GetMeta("enforcement"); v == "on" {
		st.Enforcement = true
		as := d.Authz.Status()
		if !as.Running {
			st.EnforcementError = as.Error
		}
	}
	if _, err := os.Stat(d.caRootPath()); err == nil {
		st.CAReady = true
	}
	return st
}

func (d *Daemon) CA(ctx context.Context) ([]byte, error) {
	return d.Router.RootCert(ctx)
}
