package registry

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRulesCRUD(t *testing.T) {
	r := openTest(t)
	rule := &Rule{Name: "postgres", Project: "shop", Source: 5432, Target: 5432, Resolve: "db.localhost", Proto: "tcp", Action: "route-tcp", Redirect: true}
	if err := r.CreateRule(rule); err != nil {
		t.Fatal(err)
	}
	if rule.ID == 0 {
		t.Fatal("missing id")
	}
	rules, err := r.ListRules()
	if err != nil || len(rules) != 1 {
		t.Fatalf("ListRules = %+v, %v", rules, err)
	}
	if rules[0].Name != "postgres" || rules[0].Action != "route-tcp" {
		t.Fatalf("roundtrip: %+v", rules[0])
	}
	ok, err := r.DeleteRule(rule.ID)
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if rules, _ := r.ListRules(); len(rules) != 0 {
		t.Fatalf("rules remain: %+v", rules)
	}
}

func TestRouteAndEvents(t *testing.T) {
	r := openTest(t)
	ch, unsub := r.Bus.Subscribe()
	defer unsub()
	route := &Route{Host: "web.localhost", ContainerID: "abc", Proto: "http", SourcePort: 3000, TargetAddr: "172.18.0.2:3000", Status: "up", Redirect: true}
	if err := r.CreateRoute(route); err != nil {
		t.Fatal(err)
	}
	route.TargetAddr = "172.18.0.3:3000"
	route.Status = "down"
	if err := r.UpsertRoute(route); err != nil {
		t.Fatal(err)
	}
	routes, err := r.ListRoutes()
	if err != nil || len(routes) != 1 || routes[0].TargetAddr != "172.18.0.3:3000" || routes[0].Status != "down" {
		t.Fatalf("routes: %+v, %v", routes, err)
	}
	ev := r.AddEvent("warn", "collision", "host collision", map[string]any{"host": "web.localhost"})
	if ev.ID == 0 {
		t.Fatal("event missing id")
	}
	select {
	case got := <-ch:
		if got.Kind != "collision" {
			t.Fatalf("bus event: %+v", got)
		}
	default:
		t.Fatal("event was not published on the bus")
	}
	evs, err := r.ListEvents(0, 10)
	if err != nil || len(evs) != 1 || evs[0].Data["host"] != "web.localhost" {
		t.Fatalf("events: %+v, %v", evs, err)
	}
}

func TestUniqueHostConstraints(t *testing.T) {
	r := openTest(t)
	if err := r.CreateRoute(&Route{Host: "a.localhost", ContainerID: "c1", Proto: "http", SourcePort: 80}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateRoute(&Route{Host: "a.localhost", ContainerID: "c2", Proto: "http", SourcePort: 80}); err == nil {
		t.Fatal("expected host uniqueness error")
	}
	if err := r.CreateRoute(&Route{Host: "a.localhost", ContainerID: "c1", Proto: "http", SourcePort: 81}); err == nil {
		t.Fatal("expected host/proto uniqueness error")
	}
	// Same container+proto+port upserts rather than duplicates.
	if err := r.UpsertRoute(&Route{Host: "a.localhost", ContainerID: "c1", Proto: "http", SourcePort: 80, Status: "up"}); err != nil {
		t.Fatal(err)
	}
	routes, _ := r.ListRoutes()
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %+v", routes)
	}
}

func TestGatewayPorts(t *testing.T) {
	r := openTest(t)
	if err := r.ReserveGatewayPort(5432, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveGatewayPort(5432, "k1"); err != nil {
		t.Fatalf("same key should be idempotent: %v", err)
	}
	if err := r.ReserveGatewayPort(5432, "k2"); err == nil {
		t.Fatal("expected conflict")
	}
	ports, err := r.ListGatewayPorts()
	if err != nil || ports[5432] != "k1" {
		t.Fatalf("ports: %+v, %v", ports, err)
	}
	if err := r.ReleaseUnusedGatewayPorts(map[string]bool{"other": true}); err != nil {
		t.Fatal(err)
	}
	if ports, _ := r.ListGatewayPorts(); len(ports) != 0 {
		t.Fatalf("unused port retained: %+v", ports)
	}
}

func TestLeasesAndMeta(t *testing.T) {
	r := openTest(t)
	l := &Lease{Name: "app", Host: "app.localhost", PID: 1234, Port: 3000}
	if err := r.CreateLease(l); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetLease(l.ID); err != nil || got.PID != 1234 {
		t.Fatalf("lease: %+v, %v", got, err)
	}
	if err := r.SetMeta("policy", "warn"); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.GetMeta("policy"); v != "warn" {
		t.Fatalf("meta = %q", v)
	}
}
