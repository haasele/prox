package hostproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, pkg string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if pkg != "" {
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	viteDir := fixture(t, `{"devDependencies":{"vite":"^7"}}`, nil)
	cases := []struct {
		dir  string
		argv []string
		want string
	}{
		{viteDir, []string{"npm", "run", "dev"}, "vite"},
		{viteDir, []string{"npx", "vite"}, "vite"},
		{t.TempDir(), []string{"vite", "--port", "3000"}, "vite"},
		{t.TempDir(), []string{"next", "dev"}, "next"},
		{t.TempDir(), []string{"nuxt", "dev"}, "nuxt"},
		{t.TempDir(), []string{"astro", "dev"}, "astro"},
		{t.TempDir(), []string{"bin/rails", "server"}, "rails"},
		{t.TempDir(), []string{"python", "manage.py", "runserver"}, "django"},
		{t.TempDir(), []string{"flask", "run"}, "flask"},
		{t.TempDir(), []string{"uvicorn", "app:app"}, "uvicorn"},
		{t.TempDir(), []string{"mix", "phx.server"}, "phoenix"},
		{t.TempDir(), []string{"php", "artisan", "serve"}, "laravel"},
		{t.TempDir(), []string{"go", "run", "."}, "go"},
		{fixture(t, `{"dependencies":{"express":"^4"}}`, nil), []string{"npm", "start"}, "express"},
		{fixture(t, `{"dependencies":{"@sveltejs/kit":"^2","vite":"^7"}}`, nil), []string{"npm", "run", "dev"}, "sveltekit"},
		{fixture(t, `{"dependencies":{"next":"^14"}}`, nil), []string{"npm", "run", "dev"}, "next"},
		{fixture(t, "", map[string]string{"manage.py": ""}), []string{"python", "manage.py"}, "django"},
		{fixture(t, "", map[string]string{"go.mod": ""}), []string{"go", "run", "."}, "go"},
	}
	for _, c := range cases {
		d := Detect(c.dir, c.argv)
		if d.Adapter.Name != c.want {
			t.Fatalf("Detect(%v) = %s (%s), want %s", c.argv, d.Adapter.Name, d.Reason, c.want)
		}
	}
}

func TestPortArgs(t *testing.T) {
	vite := Detection{Adapter: adapterByName("vite")}
	got := PortArgs([]string{"npm", "run", "dev"}, vite, 3000, "")
	want := []string{"--", "--port", "3000", "--host", "127.0.0.1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("npm args = %v, want %v", got, want)
	}
	got = PortArgs([]string{"vite"}, vite, 3000, "")
	if strings.Join(got, " ") != "--port 3000 --host 127.0.0.1" {
		t.Fatalf("direct vite args = %v", got)
	}
	got = PortArgs([]string{"npm", "run", "dev"}, Detection{Adapter: adapterByName("next")}, 4000, "")
	if strings.Join(got, " ") != "-- -p 4000 -H 127.0.0.1" {
		t.Fatalf("next args = %v", got)
	}
	got = PortArgs([]string{"python", "manage.py", "runserver"}, Detection{Adapter: adapterByName("django")}, 8000, "")
	if strings.Join(got, " ") != "127.0.0.1:8000" {
		t.Fatalf("django args = %v", got)
	}
	got = PortArgs([]string{"npm", "run", "dev"}, vite, 3000, "--listen={host}:{port}")
	if strings.Join(got, " ") != "-- --listen=127.0.0.1:3000" {
		t.Fatalf("override args = %v", got)
	}
	got = PortArgs([]string{"go", "run", "."}, Detection{Adapter: adapterByName("go")}, 8080, "")
	if len(got) != 0 {
		t.Fatalf("go should be env only, got %v", got)
	}
}

func TestEnv(t *testing.T) {
	env := Env(5173, "app.localhost", Detection{Adapter: adapterByName("vite")})
	joined := strings.Join(env, " ")
	for _, want := range []string{"PORT=5173", "HOST=127.0.0.1", "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=app.localhost"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env %v missing %s", env, want)
		}
	}
}
