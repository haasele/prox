package daemon

import (
	"context"

	"github.com/haasele/prox/internal/router"
)

func (d *Daemon) syncRouter(ctx context.Context) error {
	routes, err := d.Reg.ListRoutes()
	if err != nil {
		return err
	}
	in := router.Input{
		HTTPPort:          d.Cfg.HTTPPort,
		HTTPSPort:         d.Cfg.HTTPSPort,
		TLSLoopbackPort:   d.Cfg.TLSLoopbackPort,
		HTTPSListen:       d.Cfg.HTTPSListen,
		DashboardHost:     "gw.localhost",
		DashboardUpstream: d.dashDial,
		DashboardURL:      "https://" + d.Cfg.HTTPSHostPort("gw.localhost") + "/",
		CAStorage:         d.Cfg.CAStorageDir,
	}
	for _, r := range routes {
		switch r.Proto {
		case "http":
			in.Routes = append(in.Routes, router.HTTPRoute{
				Host: r.Host, Upstream: r.TargetAddr, Status: r.Status, Redirect: r.Redirect,
			})
		case "sni":
			if r.Status == "up" {
				in.SNI = append(in.SNI, router.SNIRoute{Host: r.Host, Upstream: r.TargetAddr, Status: r.Status})
			}
		case "tcp":
			if r.GatewayPort > 0 && r.TargetAddr != "" {
				in.TCP = append(in.TCP, router.TCPRoute{Port: r.GatewayPort, Upstream: r.TargetAddr, Status: r.Status})
			}
		}
	}
	return d.Router.Sync(ctx, in)
}
