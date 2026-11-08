package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/profiles"
	"gopkg.in/yaml.v3"
)

const (
	ActionPublish   = "publish"
	ActionRouteHTTP = "route-http"
	ActionRouteTCP  = "route-tcp"
	ActionDeny      = "deny"
	ActionWarn      = "warn"
	ActionSkip      = "skip"
)

const DashboardHost = "gw.localhost"

const (
	PolicyRewrite = "rewrite"
	PolicyDeny    = "deny"
	PolicyWarn    = "warn"
)

type PortBinding struct {
	HostIP   string
	HostPort string
}

type CreateInfo struct {
	Name              string
	Project           string
	Service           string
	Image             string
	Labels            map[string]string
	Bindings          map[int][]PortBinding
	ExposedPorts      []int
	ImageExposedPorts []int
	PublishAll        bool
	NetworkMode       string
}

type Decision struct {
	Action               string `json:"action"`
	Host                 string `json:"host,omitempty"`
	SourcePort           int    `json:"source_port,omitempty"`
	TargetPort           int    `json:"target_port,omitempty"` // gateway port; 443 means shared HTTPS/SNI
	PreferredGatewayPort int    `json:"preferred_gateway_port,omitempty"`
	Proto                string `json:"proto,omitempty"` // http | tcp | sni
	RuleID               int64  `json:"rule_id,omitempty"`
	Redirect             bool   `json:"redirect"`
	Publish              bool   `json:"publish"`
	Collision            bool   `json:"collision"`
	ExplicitTarget       bool   `json:"explicit_target,omitempty"`
	Profile              string `json:"profile,omitempty"`
	Reason               string `json:"reason,omitempty"`
}

type Engine struct {
	Reg      *registry.Registry
	Profiles *profiles.Set
	Domain   string
}

func New(reg *registry.Registry, set *profiles.Set, domain string) *Engine {
	if domain == "" {
		domain = "localhost"
	}
	return &Engine{Reg: reg, Profiles: set, Domain: domain}
}

func (e *Engine) Policy() string {
	v, _ := e.Reg.GetMeta("policy")
	switch v {
	case PolicyDeny, PolicyWarn, PolicyRewrite:
		return v
	}
	return PolicyRewrite
}

func (e *Engine) SetPolicy(p string) error {
	if p != PolicyRewrite && p != PolicyDeny && p != PolicyWarn {
		return fmt.Errorf("unknown policy %q (want rewrite, deny or warn)", p)
	}
	return e.Reg.SetMeta("policy", p)
}

func (e *Engine) ProfileForImage(image string) (profiles.Profile, bool) {
	return e.Profiles.MatchImage(image)
}

// Decide applies rules and the default policy to a container create request.
func (e *Engine) Decide(info CreateInfo) (Decision, error) {
	if info.Service == "" {
		info.Service = strings.TrimPrefix(info.Name, "/")
	}
	profile, hasProfile := e.Profiles.MatchImage(info.Image)
	rule, kind, ok, err := e.bestRule(info, profile, hasProfile)
	if err != nil {
		return Decision{}, err
	}
	policy := e.Policy()
	if ok {
		switch rule.Action {
		case ActionDeny:
			return Decision{Action: ActionDeny, RuleID: rule.ID, Reason: fmt.Sprintf("denied by rule %d (%s)", rule.ID, rule.Name)}, nil
		case ActionPublish:
			return Decision{Action: ActionPublish, RuleID: rule.ID, Publish: true, Redirect: true,
				Reason: fmt.Sprintf("published by rule %d (%s)", rule.ID, rule.Name)}, nil
		case ActionRouteHTTP:
			return e.httpDecision(info, &rule, profile, hasProfile)
		case ActionRouteTCP:
			return e.tcpDecision(info, &rule, profile, hasProfile)
		default:
			return Decision{}, fmt.Errorf("rule %d has unknown action %q", rule.ID, rule.Action)
		}
	}
	_ = kind

	if incompatibleNetworkMode(info.NetworkMode) {
		switch policy {
		case PolicyWarn:
			return Decision{Action: ActionWarn, Reason: "host/container network bypasses the gateway; left published"}, nil
		case PolicyDeny:
			return Decision{Action: ActionDeny, Reason: "network mode " + info.NetworkMode + " bypasses the prox gateway"}, nil
		default:
			return Decision{Action: ActionDeny, Reason: "network mode " + info.NetworkMode + " cannot be proxied; use the default or bridge network"}, nil
		}
	}

	ports := e.allPorts(info)
	publishRequested := len(info.Bindings) > 0 || info.PublishAll
	switch policy {
	case PolicyWarn:
		if publishRequested {
			return Decision{Action: ActionWarn, Reason: "no rule matched; host ports left untouched (warn policy)"}, nil
		}
		return Decision{Action: ActionSkip, Reason: "no ports to proxy"}, nil
	case PolicyDeny:
		if publishRequested {
			return Decision{Action: ActionDeny, Reason: denyReason(info, ports)}, nil
		}
		return Decision{Action: ActionSkip, Reason: "container publishes no host ports"}, nil
	default:
		return e.defaultRewrite(info, profile, hasProfile, ports, publishRequested)
	}
}

func denyReason(info CreateInfo, ports []int) string {
	name := info.Service
	if name == "" {
		name = strings.TrimPrefix(info.Name, "/")
	}
	var ps []string
	for _, p := range ports {
		ps = append(ps, strconv.Itoa(p))
	}
	portList := strings.Join(ps, ", ")
	if portList == "" {
		portList = "unknown"
	}
	return fmt.Sprintf("policy is 'deny': %s publishes host port(s) %s without a matching rule. Add one with: gw allow %s --source %s", name, portList, name, firstOr(ps, "PORT"))
}

func firstOr(v []string, def string) string {
	if len(v) > 0 {
		return v[0]
	}
	return def
}

func incompatibleNetworkMode(mode string) bool {
	return mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:")
}

func (e *Engine) defaultRewrite(info CreateInfo, profile profiles.Profile, hasProfile bool, ports []int, publishRequested bool) (Decision, error) {
	if hasProfile && profile.Proto == "tcp" {
		source := profile.Port
		if source == 0 || !containsInt(ports, source) {
			if containsInt(ports, profile.Port) || len(ports) == 0 {
				source = profile.Port
			}
		}
		if source == 0 {
			source = detectTCPPort(ports)
		}
		if source != 0 {
			host := DefaultHost(serviceName(info), info.Project, e.Domain)
			target := profile.GatewayPort
			if target == 0 {
				target = profile.Port
			}
			d := Decision{
				Action:               ActionRouteTCP,
				Host:                 host,
				SourcePort:           source,
				TargetPort:           target,
				PreferredGatewayPort: target,
				Proto:                "tcp",
				Profile:              profile.Name,
				Reason:               "profile " + profile.Name + " default routing",
			}
			if target == 443 {
				if !profile.SNI {
					return Decision{}, fmt.Errorf("%s cannot be SNI-routed on 443; use a dedicated gateway port", profile.Name)
				}
				d.Proto = "sni"
			}
			return d, nil
		}
	}
	source := e.detectHTTPPort(info)
	reason := "default rewrite policy"
	if source == 0 {
		reason = "no exposed HTTP port; will probe the container network namespace at start"
	}
	return Decision{
		Action:     ActionRouteHTTP,
		Host:       DefaultHost(serviceName(info), info.Project, e.Domain),
		SourcePort: source,
		TargetPort: 443,
		Proto:      "http",
		Redirect:   true,
		Profile:    profileName(hasProfile, profile),
		Reason:     reason,
	}, nil
}

func (e *Engine) httpDecision(info CreateInfo, rule *registry.Rule, profile profiles.Profile, hasProfile bool) (Decision, error) {
	source := rule.Source
	if source == 0 {
		source = e.detectHTTPPort(info)
	}
	if source == 0 {
		return Decision{Action: ActionRouteHTTP, Host: hostOrResolve(rule, info, e),
			TargetPort: 443, Proto: "http", RuleID: rule.ID, Redirect: rule.Redirect,
			Reason: fmt.Sprintf("rule %d matched; port will be probed at start", rule.ID)}, nil
	}
	host := rule.Resolve
	if host == "" {
		host = DefaultHost(serviceName(info), info.Project, e.Domain)
	}
	return Decision{
		Action:     ActionRouteHTTP,
		Host:       host,
		SourcePort: source,
		TargetPort: 443,
		Proto:      "http",
		RuleID:     rule.ID,
		Redirect:   rule.Redirect,
		Profile:    profileName(hasProfile, profile),
		Reason:     fmt.Sprintf("rule %d (%s)", rule.ID, rule.Name),
	}, nil
}

func (e *Engine) tcpDecision(info CreateInfo, rule *registry.Rule, profile profiles.Profile, hasProfile bool) (Decision, error) {
	source := rule.Source
	if source == 0 && hasProfile {
		source = profile.Port
	}
	if source == 0 {
		source = detectTCPPort(e.allPorts(info))
	}
	if source == 0 {
		return Decision{Action: ActionSkip, RuleID: rule.ID, Reason: fmt.Sprintf("rule %d matched but no TCP port was detected", rule.ID)}, nil
	}
	target := rule.Target
	if target == 0 && hasProfile {
		target = profile.GatewayPort
	}
	if target == 0 {
		target = 0 // daemon allocates
	}
	proto := "tcp"
	if target == 443 {
		if !hasProfile || !profile.SNI {
			name := rule.Name
			if hasProfile {
				name = profile.Name
			}
			return Decision{}, fmt.Errorf("%s cannot be SNI-routed on port 443: protocol %q does not carry a routable SNI name. Omit --target/--gateway-port to use a dedicated gateway port (e.g. %d)", name, name, source)
		}
		proto = "sni"
	}
	host := rule.Resolve
	if host == "" {
		host = DefaultHost(serviceName(info), info.Project, e.Domain)
	}
	return Decision{
		Action:               ActionRouteTCP,
		Host:                 host,
		SourcePort:           source,
		TargetPort:           target,
		PreferredGatewayPort: target,
		Proto:                proto,
		RuleID:               rule.ID,
		Redirect:             rule.Redirect,
		Profile:              profileName(hasProfile, profile),
		ExplicitTarget:       rule.TargetExplicit,
		Reason:               fmt.Sprintf("rule %d (%s)", rule.ID, rule.Name),
	}, nil
}

func hostOrResolve(rule *registry.Rule, info CreateInfo, e *Engine) string {
	if rule.Resolve != "" {
		return rule.Resolve
	}
	return DefaultHost(serviceName(info), info.Project, e.Domain)
}

func profileName(ok bool, p profiles.Profile) string {
	if ok {
		return p.Name
	}
	return ""
}

func serviceName(info CreateInfo) string {
	if info.Service != "" {
		return info.Service
	}
	return strings.TrimPrefix(info.Name, "/")
}

func (e *Engine) detectHTTPPort(info CreateInfo) int {
	if v := info.Labels["gw.port"]; v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}
	// The spec's order: image EXPOSE before create-body ports.
	if p := DetectHTTPPort(info.ImageExposedPorts); p != 0 {
		return p
	}
	return DetectHTTPPort(e.allPorts(info))
}

func (e *Engine) allPorts(info CreateInfo) []int {
	seen := map[int]bool{}
	var out []int
	for p := range info.Bindings {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range info.ExposedPorts {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range info.ImageExposedPorts {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

var httpPortPreference = []int{80, 443, 8080, 3000, 3001, 5173, 8000, 5000, 4000, 4200, 8025, 9000, 9090, 15672, 9200, 2368}

func DetectHTTPPort(ports []int) int {
	if len(ports) == 0 {
		return 0
	}
	for _, pref := range httpPortPreference {
		if containsInt(ports, pref) {
			return pref
		}
	}
	lowest := ports[0]
	for _, p := range ports {
		if p < lowest {
			lowest = p
		}
	}
	return lowest
}

func detectTCPPort(ports []int) int {
	if len(ports) == 0 {
		return 0
	}
	lowest := ports[0]
	for _, p := range ports {
		if p < lowest {
			lowest = p
		}
	}
	return lowest
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// bestRule selects by (project scoped, match specificity, id).
func (e *Engine) bestRule(info CreateInfo, profile profiles.Profile, hasProfile bool) (registry.Rule, int, bool, error) {
	rules, err := e.Reg.ListRules()
	if err != nil {
		return registry.Rule{}, 0, false, err
	}
	bestScore := -1
	var best registry.Rule
	bestKind := 0
	for _, r := range rules {
		if r.Project != "" && !strings.EqualFold(r.Project, info.Project) {
			continue
		}
		kind, ok := matchKind(r.Name, info, profile, hasProfile)
		if !ok {
			continue
		}
		score := kind * 10
		if r.Project != "" {
			score += 100
		}
		if score > bestScore || (score == bestScore && r.ID > best.ID) {
			best, bestScore, bestKind = r, score, kind
		}
	}
	if bestScore < 0 {
		return registry.Rule{}, 0, false, nil
	}
	return best, bestKind, true, nil
}

func matchKind(name string, info CreateInfo, profile profiles.Profile, hasProfile bool) (int, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false
	}
	if info.Service != "" && strings.EqualFold(name, info.Service) {
		return 4, true
	}
	cname := strings.TrimPrefix(info.Name, "/")
	if cname != "" && strings.EqualFold(name, cname) {
		return 3, true
	}
	for _, c := range imageVariants(info.Image) {
		if strings.EqualFold(name, c) {
			return 2, true
		}
	}
	if hasProfile && strings.EqualFold(name, profile.Name) {
		return 1, true
	}
	return 0, false
}

func imageVariants(image string) []string {
	image = strings.ToLower(strings.TrimSpace(image))
	base := image
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	baseNoTag := base
	if i := strings.LastIndex(baseNoTag, ":"); i >= 0 {
		baseNoTag = baseNoTag[:i]
	}
	noTag := image
	if i := strings.LastIndex(noTag, ":"); i > strings.LastIndex(noTag, "/") {
		noTag = noTag[:i]
	}
	return []string{image, noTag, base, baseNoTag}
}

// ---- Rule creation ----

type RuleRequest struct {
	Name     string
	Project  string
	Source   int
	Target   int
	Resolve  string
	Proto    string
	Action   string // allow | deny | publish
	Redirect *bool
}

func (e *Engine) AddRule(req RuleRequest) (*registry.Rule, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("rule name is required")
	}
	if req.Source < 0 || req.Source > 65535 || req.Target < 0 || req.Target > 65535 {
		return nil, errors.New("port must be between 1 and 65535")
	}
	profile, hasProfile := e.Profiles.Get(req.Name)
	if !hasProfile {
		profile, hasProfile = e.Profiles.MatchImage(req.Name)
	}

	action := req.Action
	if action == "" || action == "allow" {
		proto := strings.ToLower(req.Proto)
		if proto == "" && hasProfile {
			proto = profile.Proto
		}
		if proto == "" {
			proto = "http"
		}
		if proto != "http" && proto != "tcp" {
			return nil, fmt.Errorf("proto must be http or tcp, got %q", proto)
		}
		if proto == "http" {
			action = ActionRouteHTTP
		} else {
			action = ActionRouteTCP
		}
		req.Proto = proto
	}
	if action != ActionRouteHTTP && action != ActionRouteTCP && action != ActionDeny && action != ActionPublish {
		return nil, fmt.Errorf("unknown action %q", action)
	}

	redirect := true
	if req.Redirect != nil {
		redirect = *req.Redirect
	}
	r := &registry.Rule{
		Name:     strings.ToLower(strings.TrimSpace(req.Name)),
		Project:  strings.ToLower(strings.TrimSpace(req.Project)),
		Resolve:  NormalizeResolve(req.Resolve, e.Domain),
		Action:   action,
		Redirect: redirect,
	}
	if r.Resolve == "" && action != ActionDeny && action != ActionPublish {
		r.Resolve = NormalizeResolve(r.Name, e.Domain)
	}
	if r.Resolve == DashboardHost {
		return nil, fmt.Errorf("%s is reserved for the prox dashboard; choose another --resolve value", DashboardHost)
	}
	r.TargetExplicit = req.Target != 0

	switch action {
	case ActionDeny:
		r.Proto = strings.ToLower(req.Proto)
	case ActionPublish:
		r.Proto = strings.ToLower(req.Proto)
	case ActionRouteHTTP:
		r.Proto = "http"
		r.Source = req.Source
		if r.Source == 0 && hasProfile {
			r.Source = profile.Port
		}
		if req.Target != 0 && req.Target != 443 {
			return nil, fmt.Errorf("HTTP rules terminate on the shared HTTPS port 443; use --proto tcp for gateway port %d", req.Target)
		}
		r.Target = 443
	case ActionRouteTCP:
		r.Proto = "tcp"
		r.Source = req.Source
		if r.Source == 0 && hasProfile {
			r.Source = profile.Port
		}
		r.Target = req.Target
		if r.Target == 0 && hasProfile {
			r.Target = profile.GatewayPort
			if r.Target == 0 {
				r.Target = profile.Port
			}
		}
		if r.Target == 0 {
			// Spec default: for TCP the gateway port defaults to --source.
			r.Target = r.Source
		}
		if r.Target == 443 {
			if !hasProfile || !profile.SNI {
				hint := r.Source
				if hint == 0 {
					hint = profile.Port
				}
				if hint == 0 {
					hint = 5432
				}
				return nil, fmt.Errorf("%s cannot be SNI-routed on port 443: protocol %q does not carry a routable SNI name. Omit --target/--gateway-port to get a dedicated gateway port (e.g. --target %d)", r.Name, r.Name, hint)
			}
		}
	}
	if r.Source == 0 && action == ActionRouteTCP && hasProfile {
		r.Source = profile.Port
	}
	if err := e.Reg.CreateRule(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (e *Engine) DeleteRule(id int64) (bool, error) {
	return e.Reg.DeleteRule(id)
}

// AllowsDirectPublish reports whether a create request may keep its host port
// bindings when enforcement is active. Only an explicit publish rule qualifies;
// routed rules must arrive through the proxy (signed).
func (e *Engine) AllowsDirectPublish(info CreateInfo) bool {
	if info.Service == "" {
		info.Service = strings.TrimPrefix(info.Name, "/")
	}
	profile, hasProfile := e.Profiles.MatchImage(info.Image)
	rule, _, ok, err := e.bestRule(info, profile, hasProfile)
	if err != nil || !ok {
		return false
	}
	return rule.Action == ActionPublish
}

// ---- YAML import/export ----

type yamlRule struct {
	ID       int64     `yaml:"id"`
	Match    yamlMatch `yaml:"match"`
	Source   int       `yaml:"source,omitempty"`
	Target   int       `yaml:"target,omitempty"`
	Resolve  string    `yaml:"resolve,omitempty"`
	Proto    string    `yaml:"proto,omitempty"`
	Action   string    `yaml:"action"`
	Redirect *bool     `yaml:"redirect,omitempty"`
}

type yamlMatch struct {
	Name    string `yaml:"name"`
	Project string `yaml:"project,omitempty"`
}

func (e *Engine) ExportRules(path string) error {
	rules, err := e.Reg.ListRules()
	if err != nil {
		return err
	}
	out := make([]yamlRule, 0, len(rules))
	for _, r := range rules {
		y := yamlRule{
			ID:      r.ID,
			Match:   yamlMatch{Name: r.Name, Project: r.Project},
			Source:  r.Source,
			Target:  r.Target,
			Resolve: r.Resolve,
			Proto:   r.Proto,
			Action:  r.Action,
		}
		if !r.Redirect {
			f := false
			y.Redirect = &f
		}
		out = append(out, y)
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) ImportRules(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var in []yamlRule
	if err := yaml.Unmarshal(b, &in); err != nil {
		return 0, err
	}
	rules := make([]registry.Rule, 0, len(in))
	for _, y := range in {
		redirect := true
		if y.Redirect != nil {
			redirect = *y.Redirect
		}
		rules = append(rules, registry.Rule{
			Name:           strings.ToLower(y.Match.Name),
			Project:        strings.ToLower(y.Match.Project),
			Source:         y.Source,
			Target:         y.Target,
			Resolve:        y.Resolve,
			Proto:          y.Proto,
			Action:         y.Action,
			Redirect:       redirect,
			TargetExplicit: y.Target > 0,
		})
	}
	if err := e.Reg.ReplaceRules(rules); err != nil {
		return 0, err
	}
	return len(rules), nil
}
