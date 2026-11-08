package backend

import (
	"strings"
	"testing"

	"github.com/haasele/prox/internal/dockerclient"
)

func TestNativeAddress(t *testing.T) {
	c := &dockerclient.Container{}
	c.NetworkSettings.Networks = map[string]*dockerclient.NetEndpoint{"gw": {IPAddress: "172.18.0.4"}}
	addr, err := (Native{}).ResolveAddress(c, 3000)
	if err != nil || addr != "172.18.0.4:3000" {
		t.Fatalf("addr=%q err=%v", addr, err)
	}
	empty := &dockerclient.Container{}
	if _, err := (Native{}).ResolveAddress(empty, 3000); err == nil {
		t.Fatal("want error for missing network")
	}
}

func TestPublishedAddress(t *testing.T) {
	c := &dockerclient.Container{}
	c.NetworkSettings.Ports = map[string][]dockerclient.PortBinding{
		"3000/tcp": {{HostIP: "127.0.0.1", HostPort: "41234"}},
	}
	addr, err := (Published{Kind: "desktop"}).ResolveAddress(c, 3000)
	if err != nil || addr != "127.0.0.1:41234" {
		t.Fatalf("addr=%q err=%v", addr, err)
	}
	if _, err := (Published{Kind: "desktop"}).ResolveAddress(c, 4000); err == nil {
		t.Fatal("want error for missing published port")
	}
}

func TestDetect(t *testing.T) {
	be, err := Detect("auto", &dockerclient.Info{SecurityOptions: []string{"name=rootless"}}, "/run/user/1000/docker.sock")
	if err != nil || be.Name() != "rootless" {
		t.Fatalf("rootless: %v %v", be, err)
	}
	be, _ = Detect("auto", &dockerclient.Info{OperatingSystem: "Docker Desktop"}, "/var/run/docker.sock")
	if be.Name() != "desktop" {
		t.Fatalf("desktop: %v", be)
	}
	be, _ = Detect("auto", &dockerclient.Info{OperatingSystem: "Ubuntu 24.04"}, "/var/run/docker.sock")
	if be.Name() != "native" {
		t.Fatalf("native: %v", be)
	}
	if _, err := Detect("bogus", nil, ""); err == nil {
		t.Fatal("want error for unknown backend")
	}
}

func TestAddLoopbackBindings(t *testing.T) {
	hc := map[string]any{}
	AddLoopbackBindings(hc, []int{3000})
	pb, _ := hc["PortBindings"].(map[string]any)
	entries, _ := pb["3000/tcp"].([]any)
	if len(entries) != 1 {
		t.Fatalf("bindings: %+v", hc)
	}
	e := entries[0].(map[string]any)
	if e["HostIp"] != "127.0.0.1" || e["HostPort"] != "" {
		t.Fatalf("entry: %+v", e)
	}
	if !strings.Contains(Published{Kind: "rootless"}.Name(), "rootless") {
		t.Fatal("name")
	}
	if (Published{Kind: "rootless"}).CanBind(443) {
		t.Fatal("rootless should not claim privileged ports")
	}
}
