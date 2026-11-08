package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltins(t *testing.T) {
	s, err := LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"elasticsearch", "mailpit", "mariadb", "memcached", "minio", "mongo", "mysql", "postgres", "rabbitmq", "redis"}
	for _, name := range want {
		if _, ok := s.Get(name); !ok {
			t.Fatalf("missing builtin profile %q", name)
		}
	}
	cases := map[string]string{
		"postgres:16":                                          "postgres",
		"docker.io/library/postgres:16":                        "postgres",
		"ghcr.io/timescale/timescaledb:latest":                 "postgres",
		"redis:7-alpine":                                       "redis",
		"quay.io/minio/minio:latest":                           "minio",
		"docker.elastic.co/elasticsearch/elasticsearch:8.12.0": "elasticsearch",
		"nginx:latest":                                         "",
	}
	for image, wantProfile := range cases {
		p, ok := s.MatchImage(image)
		if wantProfile == "" {
			if ok {
				t.Fatalf("%s unexpectedly matched %s", image, p.Name)
			}
			continue
		}
		if !ok || p.Name != wantProfile {
			t.Fatalf("%s matched %q, want %q", image, p.Name, wantProfile)
		}
	}
}

func TestUserOverride(t *testing.T) {
	base, err := LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "postgres.yaml"), []byte(`
name: postgres
images:
  - postgres
port: 15432
proto: tcp
sni: false
gateway_port: 15432
`), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err := base.LoadUserDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := merged.Get("postgres")
	if !ok || p.Port != 15432 || p.SNI {
		t.Fatalf("override not applied: %+v ok=%v", p, ok)
	}
	// The original set is unchanged.
	if orig, _ := base.Get("postgres"); orig.Port != 5432 {
		t.Fatalf("builtin set mutated: %+v", orig)
	}
}

func TestInvalidProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("name: bad\nproto: udp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, _ := LoadBuiltins()
	if _, err := base.LoadUserDir(dir); err == nil {
		t.Fatal("expected invalid proto error")
	}
}
