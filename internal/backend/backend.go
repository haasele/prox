package backend

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/haasele/prox/internal/dockerclient"
)

// Backend abstracts how the host reaches a container. The policy and router
// only see host:port targets, so native, rootless and Docker Desktop can all
// share the same routing logic.
type Backend interface {
	Name() string
	// RequiresPublishedPorts reports whether create requests need a
	// loopback-only ephemeral publication for the routed ports, because
	// container IPs are not reachable from the host.
	RequiresPublishedPorts() bool
	// ResolveAddress returns a host:port reachable from the host for the
	// given container port.
	ResolveAddress(c *dockerclient.Container, port int) (string, error)
	// CanBind reports whether the gateway may listen on this port.
	CanBind(port int) bool
}

type Native struct{}

func (Native) Name() string                 { return "native" }
func (Native) RequiresPublishedPorts() bool { return false }
func (Native) CanBind(port int) bool        { return port > 0 && port <= 65535 }

func (Native) ResolveAddress(c *dockerclient.Container, port int) (string, error) {
	if c.NetworkSettings.Networks != nil {
		if ep, ok := c.NetworkSettings.Networks["gw"]; ok && ep.IPAddress != "" {
			return net.JoinHostPort(ep.IPAddress, strconv.Itoa(port)), nil
		}
	}
	return "", fmt.Errorf("container %s has no address on the gw network", strings.TrimPrefix(c.Name, "/"))
}

// Published implements rootless and Docker Desktop: a loopback-only ephemeral
// host port is published internally for each routed container port, and Caddy
// dials 127.0.0.1:<port>. This is still invisible to Compose because inspect
// responses are restored to the original create body.
type Published struct {
	Kind string // rootless | desktop
}

func (p Published) Name() string                 { return p.Kind }
func (p Published) RequiresPublishedPorts() bool { return true }

func (p Published) CanBind(port int) bool {
	// Rootless has no privileged port access unless the administrator lowered
	// net.ipv4.ip_unprivileged_port_start.
	return port > 1024 && port <= 65535
}

func (p Published) ResolveAddress(c *dockerclient.Container, port int) (string, error) {
	key := strconv.Itoa(port) + "/tcp"
	binds := c.NetworkSettings.Ports[key]
	for _, b := range binds {
		if b.HostPort != "" {
			host := b.HostIP
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			return net.JoinHostPort(host, b.HostPort), nil
		}
	}
	return "", fmt.Errorf("container %s has no published loopback port for %d/tcp", strings.TrimPrefix(c.Name, "/"), port)
}

// Detect picks a backend. explicit may be "", "auto", "native", "rootless" or
// "desktop".
func Detect(explicit string, info *dockerclient.Info, socket string) (Backend, error) {
	switch explicit {
	case "native":
		return Native{}, nil
	case "rootless":
		return Published{Kind: "rootless"}, nil
	case "desktop":
		return Published{Kind: "desktop"}, nil
	case "", "auto":
	default:
		return nil, fmt.Errorf("unknown backend %q (want native, rootless or desktop)", explicit)
	}
	if info != nil {
		for _, opt := range info.SecurityOptions {
			if strings.Contains(strings.ToLower(opt), "rootless") {
				return Published{Kind: "rootless"}, nil
			}
		}
		if strings.Contains(strings.ToLower(info.OperatingSystem), "docker desktop") ||
			strings.Contains(strings.ToLower(info.Name), "docker-desktop") {
			return Published{Kind: "desktop"}, nil
		}
	}
	if strings.Contains(socket, "desktop") {
		return Published{Kind: "desktop"}, nil
	}
	return Native{}, nil
}

// AddLoopbackBindings adds 127.0.0.1:0 bindings for routed ports in a create
// body's HostConfig. Docker picks ephemeral host ports which are discovered by
// inspecting the started container.
func AddLoopbackBindings(hostConfig map[string]any, ports []int) {
	if len(ports) == 0 {
		return
	}
	pb, _ := hostConfig["PortBindings"].(map[string]any)
	if pb == nil {
		pb = map[string]any{}
		hostConfig["PortBindings"] = pb
	}
	for _, p := range ports {
		key := strconv.Itoa(p) + "/tcp"
		pb[key] = []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": ""}}
	}
}
