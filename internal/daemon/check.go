package daemon

import (
	"context"
	"net"
	"strconv"

	"github.com/haasele/prox/internal/composecheck"
)

func (d *Daemon) CheckCompose(ctx context.Context, body []byte, filename, project string) ([]composecheck.Outcome, error) {
	reserved, _ := d.Reg.ListGatewayPorts()
	portFree := func(port int) bool {
		if _, ok := reserved[port]; ok {
			return false
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			return false
		}
		_ = ln.Close()
		return true
	}
	return composecheck.Check(d.Engine, d.Reg, body, filename, project, portFree)
}
