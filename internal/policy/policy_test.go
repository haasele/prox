package policy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/profiles"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set, err := profiles.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	return New(reg, set, "localhost")
}

func TestNaming(t *testing.T) {
	if got := SanitizeLabel("My_App!!"); got != "my-app" {
		t.Fatalf("SanitizeLabel = %q", got)
	}
	if got := SanitizeLabel(""); got != "app" {
		t.Fatalf("empty SanitizeLabel = %q", got)
	}
	long := SanitizeLabel(strings.Repeat("a", 100))
	if len(long) != 63 {
		t.Fatalf("long label length = %d", len(long))
	}
	if got := NormalizeResolve("database", "localhost"); got != "database.localhost" {
		t.Fatalf("NormalizeResolve = %q", got)
	}
	if got := NormalizeResolve("Database.Localhost", "localhost"); got != "database.localhost" {
		t.Fatalf("NormalizeResolve full = %q", got)
	}
	if got := DefaultHost("web", "shop", "localhost"); got != "web.shop.localhost" {
		t.Fatalf("DefaultHost = %q", got)
	}
	if got := DefaultHost("web", "", "localhost"); got != "web.localhost" {
		t.Fatalf("DefaultHost empty project = %q", got)
	}
	if got := CollisionHost("database.localhost", "shop", "localhost"); got != "database.shop.localhost" {
		t.Fatalf("CollisionHost = %q", got)
	}
	taken := map[string]bool{"a.localhost": true, "a-shop.localhost": true}
	if got := UniqueHost("a-shop.localhost", func(h string) bool { return taken[h] }); got != "a-shop-2.localhost" {
		t.Fatalf("UniqueHost = %q", got)
	}
}

func TestPolicyPrecedence(t *testing.T) {
	e := newEngine(t)
	if _, err := e.AddRule(RuleRequest{Name: "web", Source: 3000, Proto: "http"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(RuleRequest{Name: "web", Project: "shop", Action: "deny"}); err != nil {
		t.Fatal(err)
	}
	d, err := e.Decide(CreateInfo{Name: "web-1", Project: "shop", Service: "web", Image: "nginx", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny {
		t.Fatalf("project deny should win, got %+v", d)
	}
	d, err = e.Decide(CreateInfo{Name: "web-1", Project: "blog", Service: "web", Image: "nginx", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionRouteHTTP || d.Host != "web.localhost" {
		t.Fatalf("global rule should win for other project, got %+v", d)
	}
}

func TestLaterRuleWins(t *testing.T) {
	e := newEngine(t)
	if _, err := e.AddRule(RuleRequest{Name: "web", Project: "shop", Source: 3000, Proto: "http"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(RuleRequest{Name: "web", Project: "shop", Action: "deny"}); err != nil {
		t.Fatal(err)
	}
	d, err := e.Decide(CreateInfo{Name: "web-1", Project: "shop", Service: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny {
		t.Fatalf("later rule should win, got %+v", d)
	}
}

func TestSpecificBeatsProfile(t *testing.T) {
	e := newEngine(t)
	// A rule matching the service is more specific than a profile rule.
	if _, err := e.AddRule(RuleRequest{Name: "postgres", Source: 5432, Proto: "tcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(RuleRequest{Name: "postgres", Project: "shop", Action: "deny"}); err != nil {
		t.Fatal(err)
	}
	// Global profile rule and project deny: project-scoped wins.
	d, err := e.Decide(CreateInfo{Name: "db", Project: "shop", Service: "database", Image: "postgres:16"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny {
		t.Fatalf("want deny, got %+v", d)
	}
}

func TestDefaultRewriteHTTP(t *testing.T) {
	e := newEngine(t)
	d, err := e.Decide(CreateInfo{Name: "nginx", Image: "nginx:latest", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionRouteHTTP || d.SourcePort != 3000 || d.Host != "nginx.localhost" || d.TargetPort != 443 {
		t.Fatalf("unexpected default decision: %+v", d)
	}
}

func TestDefaultRewritePostgresProfile(t *testing.T) {
	e := newEngine(t)
	d, err := e.Decide(CreateInfo{Name: "pg", Image: "docker.io/library/postgres:16", Bindings: map[int][]PortBinding{5432: {{HostPort: "5432"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionRouteTCP || d.SourcePort != 5432 || d.TargetPort != 5432 || d.Proto != "tcp" || d.Profile != "postgres" {
		t.Fatalf("unexpected postgres decision: %+v", d)
	}
}

func TestPolicyDenyAndWarn(t *testing.T) {
	e := newEngine(t)
	if err := e.SetPolicy(PolicyDeny); err != nil {
		t.Fatal(err)
	}
	d, err := e.Decide(CreateInfo{Name: "web", Image: "nginx", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny || !strings.Contains(d.Reason, "gw allow") {
		t.Fatalf("want deny with hint, got %+v", d)
	}
	if _, err := e.AddRule(RuleRequest{Name: "web", Source: 3000, Proto: "http"}); err != nil {
		t.Fatal(err)
	}
	d, err = e.Decide(CreateInfo{Name: "web", Image: "nginx", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionRouteHTTP {
		t.Fatalf("explicit rule should override deny policy: %+v", d)
	}
	if err := e.SetPolicy(PolicyWarn); err != nil {
		t.Fatal(err)
	}
	d, err = e.Decide(CreateInfo{Name: "other", Image: "nginx", Bindings: map[int][]PortBinding{3000: {{HostPort: "3000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionWarn {
		t.Fatalf("want warn, got %+v", d)
	}
}

func TestAddRuleDefaultsAndSNI(t *testing.T) {
	e := newEngine(t)
	r, err := e.AddRule(RuleRequest{Name: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != 5432 || r.Target != 5432 || r.Proto != "tcp" || r.Action != ActionRouteTCP || r.Resolve != "postgres.localhost" {
		t.Fatalf("postgres defaults: %+v", r)
	}
	if r.TargetExplicit {
		t.Fatal("profile default target must not be explicit")
	}
	d, err := e.Decide(CreateInfo{Name: "db", Project: "shop", Service: "db", Image: "postgres:16"})
	if err != nil {
		t.Fatal(err)
	}
	if d.ExplicitTarget {
		t.Fatalf("decision should allow gateway fallback: %+v", d)
	}
	if _, err := e.AddRule(RuleRequest{Name: "redis", Target: 443}); err == nil || !strings.Contains(err.Error(), "SNI") {
		t.Fatalf("redis target 443 error = %v", err)
	}
	r, err = e.AddRule(RuleRequest{Name: "postgres", Target: 443, Resolve: "database"})
	if err != nil {
		t.Fatalf("postgres SNI rule: %v", err)
	}
	if r.Target != 443 || r.Resolve != "database.localhost" || !r.TargetExplicit {
		t.Fatalf("postgres SNI: %+v", r)
	}
	if _, err := e.AddRule(RuleRequest{Name: "web", Source: 3000, Target: 8080}); err == nil {
		t.Fatal("HTTP non-443 target should error")
	}
	if _, err := e.AddRule(RuleRequest{Name: "x", Source: 70000}); err == nil {
		t.Fatal("out of range port should error")
	}
	if _, err := e.AddRule(RuleRequest{Name: "x", Resolve: "gw.localhost"}); err == nil {
		t.Fatal("dashboard hostname must be reserved")
	}
}

func TestRuleExportImport(t *testing.T) {
	e := newEngine(t)
	if _, err := e.AddRule(RuleRequest{Name: "postgres", Resolve: "db", Project: "shop"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := e.ExportRules(p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(RuleRequest{Name: "redis"}); err != nil {
		t.Fatal(err)
	}
	n, err := e.ImportRules(p)
	if err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	rules, _ := e.Reg.ListRules()
	if len(rules) != 1 || rules[0].Name != "postgres" || rules[0].Project != "shop" || rules[0].Resolve != "db.localhost" {
		t.Fatalf("imported rules: %+v", rules)
	}
}

func TestDetectHTTPPort(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{[]int{5432, 3000}, 3000},
		{[]int{8080, 80}, 80},
		{[]int{1234, 2345}, 1234},
		{nil, 0},
	}
	for _, c := range cases {
		if got := DetectHTTPPort(c.in); got != c.want {
			t.Fatalf("DetectHTTPPort(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
