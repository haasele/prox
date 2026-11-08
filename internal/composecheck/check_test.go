package composecheck

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/profiles"
)

func TestCheck(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "check.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set, err := profiles.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	eng := policy.New(reg, set, "localhost")
	if _, err := eng.AddRule(policy.RuleRequest{Name: "nginx", Source: 80, Resolve: "app"}); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`
name: shop
services:
  web:
    image: nginx:latest
    ports: ["3000:80"]
  api:
    image: nginx:latest
    ports: ["3001:80"]
  db:
    image: postgres:16
    ports: ["5432:5432"]
  worker:
    image: alpine:3.22
    command: ["sleep", "1"]
`)
	dir := t.TempDir()
	out, err := Check(eng, reg, yaml, filepath.Join(dir, "compose.yml"), "", func(port int) bool { return port != 5432 })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("outcomes: %+v", out)
	}
	byName := map[string]Outcome{}
	for _, o := range out {
		byName[o.Service] = o
	}
	if byName["api"].Status != "routed-http" || byName["api"].Host != "app.localhost" {
		t.Fatalf("api: %+v", byName["api"])
	}
	if byName["web"].Status != "routed-http" || byName["web"].Host != "app.shop.localhost" || !byName["web"].Collision {
		t.Fatalf("web collision: %+v", byName["web"])
	}
	if byName["db"].Status != "routed-tcp" || byName["db"].SourcePort != 5432 || !byName["db"].GatewayBusy {
		t.Fatalf("db: %+v", byName["db"])
	}
	if byName["worker"].Status != "routed-http" || byName["worker"].SourcePort != 0 || !strings.Contains(byName["worker"].Reason, "probe") {
		t.Fatalf("worker: %+v", byName["worker"])
	}
}

func TestCheckDenyPolicy(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "check2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set, _ := profiles.LoadBuiltins()
	eng := policy.New(reg, set, "localhost")
	if err := eng.SetPolicy(policy.PolicyDeny); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`
services:
  web:
    image: nginx
    ports: ["3000:80"]
`)
	out, err := Check(eng, reg, yaml, filepath.Join(t.TempDir(), "compose.yml"), "shop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Status != "denied" || out[0].Project != "shop" {
		t.Fatalf("outcomes: %+v", out)
	}
}
