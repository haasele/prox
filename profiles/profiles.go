// Package profiles contains the built-in service profiles and the loader for
// user overrides from /etc/prox/profiles.d.
package profiles

import (
	"embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed *.yaml
var builtinFS embed.FS

type Profile struct {
	Name        string   `yaml:"name" json:"name"`
	Images      []string `yaml:"images" json:"images"`
	Port        int      `yaml:"port" json:"port"`
	Proto       string   `yaml:"proto" json:"proto"` // http | tcp
	SNI         bool     `yaml:"sni" json:"sni"`
	GatewayPort int      `yaml:"gateway_port" json:"gateway_port"`
	Healthcheck string   `yaml:"healthcheck" json:"healthcheck,omitempty"`
}

type Set struct {
	ByName map[string]Profile
}

func LoadBuiltins() (*Set, error) {
	s := &Set{ByName: map[string]Profile{}}
	entries, err := builtinFS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		b, err := builtinFS.ReadFile(ent.Name())
		if err != nil {
			return nil, err
		}
		var p Profile
		if err := yaml.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("builtin profile %s: %w", ent.Name(), err)
		}
		if err := p.validate(ent.Name()); err != nil {
			return nil, err
		}
		s.ByName[p.Name] = p
	}
	return s, nil
}

// LoadUserDir overlays user profiles on a copy of the built-in set. A user
// profile with the same name replaces the built-in one completely.
func (s *Set) LoadUserDir(dir string) (*Set, error) {
	out := &Set{ByName: map[string]Profile{}}
	for k, v := range s.ByName {
		out.ByName[k] = v
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		var p Profile
		if err := yaml.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("profile %s: %w", ent.Name(), err)
		}
		if err := p.validate(ent.Name()); err != nil {
			return nil, err
		}
		out.ByName[p.Name] = p
	}
	return out, nil
}

func (p *Profile) validate(src string) error {
	if p.Name == "" {
		return fmt.Errorf("profile %s: name is required", src)
	}
	if p.Port < 0 || p.Port > 65535 || p.GatewayPort < 0 || p.GatewayPort > 65535 {
		return fmt.Errorf("profile %s: invalid port", src)
	}
	if p.Proto == "" {
		p.Proto = "http"
	}
	if p.Proto != "http" && p.Proto != "tcp" {
		return fmt.Errorf("profile %s: proto must be http or tcp", src)
	}
	return nil
}

func (s *Set) Get(name string) (Profile, bool) {
	p, ok := s.ByName[strings.ToLower(name)]
	return p, ok
}

func (s *Set) Names() []string {
	out := make([]string, 0, len(s.ByName))
	for k := range s.ByName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MatchImage returns the first profile whose image patterns match. The set is
// sorted by name for deterministic results.
func (s *Set) MatchImage(image string) (Profile, bool) {
	if image == "" {
		return Profile{}, false
	}
	names := s.Names()
	candidates := imageCandidates(image)
	for _, name := range names {
		p := s.ByName[name]
		for _, pat := range p.Images {
			pat = strings.ToLower(strings.TrimSpace(pat))
			if pat == "" {
				continue
			}
			for _, c := range candidates {
				if pat == c {
					return p, true
				}
				if ok, _ := path.Match(pat, c); ok {
					return p, true
				}
				// Images on a non-Docker-Hub registry carry a registry
				// prefix; also match the repo path without it.
				if strings.Contains(pat, "/") && strings.HasSuffix(c, "/"+pat) {
					return p, true
				}
			}
		}
	}
	return Profile{}, false
}

func imageCandidates(image string) []string {
	image = strings.ToLower(strings.TrimSpace(image))
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	full := image
	noTag := full
	if i := strings.LastIndex(noTag, ":"); i > strings.LastIndex(noTag, "/") {
		noTag = noTag[:i]
	}
	base := full
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	baseNoTag := base
	if i := strings.LastIndex(baseNoTag, ":"); i >= 0 {
		baseNoTag = baseNoTag[:i]
	}
	// Docker Hub shorthand: docker.io/library/postgres -> postgres.
	short := strings.TrimPrefix(noTag, "docker.io/library/")
	short = strings.TrimPrefix(short, "index.docker.io/library/")
	short = strings.TrimPrefix(short, "docker.io/")
	out := []string{full, noTag, base, baseNoTag, short}
	seen := map[string]bool{}
	uniq := out[:0]
	for _, v := range out {
		if v != "" && !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return uniq
}
