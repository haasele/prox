package hostproc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Adapter struct {
	Name        string
	DefaultPort int      // conventional dev port, used for an original-port hint
	Flags       []string // {port} and {host} are substituted
	Positional  string   // appended as one argument, e.g. 127.0.0.1:{port}
	EnvOnly     bool
}

type Detection struct {
	Adapter Adapter
	Reason  string
}

var viteFlags = []string{"--port", "{port}", "--host", "127.0.0.1"}

var adapters = []Adapter{
	{Name: "vite", DefaultPort: 5173, Flags: viteFlags},
	{Name: "next", DefaultPort: 3000, Flags: []string{"-p", "{port}", "-H", "127.0.0.1"}},
	{Name: "nuxt", DefaultPort: 3000, Flags: []string{"--port", "{port}", "--host", "127.0.0.1"}},
	{Name: "sveltekit", DefaultPort: 5173, Flags: viteFlags},
	{Name: "astro", DefaultPort: 4321, Flags: []string{"--port", "{port}", "--host", "127.0.0.1"}},
	{Name: "rails", DefaultPort: 3000, Flags: []string{"-p", "{port}", "-b", "127.0.0.1"}},
	{Name: "django", DefaultPort: 8000, Positional: "127.0.0.1:{port}"},
	{Name: "flask", DefaultPort: 5000, Flags: []string{"--port", "{port}", "--host", "127.0.0.1"}},
	{Name: "uvicorn", DefaultPort: 8000, Flags: []string{"--port", "{port}", "--host", "127.0.0.1"}},
	{Name: "express", DefaultPort: 3000, EnvOnly: true},
	{Name: "phoenix", DefaultPort: 4000, EnvOnly: true},
	{Name: "laravel", DefaultPort: 8000, Flags: []string{"--port={port}", "--host=127.0.0.1"}},
	{Name: "go", DefaultPort: 8080, EnvOnly: true},
}

func adapterByName(name string) Adapter {
	for _, a := range adapters {
		if a.Name == name {
			return a
		}
	}
	return Adapter{Name: "generic", DefaultPort: 3000, EnvOnly: true}
}

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Scripts         map[string]string `json:"scripts"`
}

func readPackage(dir string) *packageJSON {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var p packageJSON
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// Detect picks a framework from the command first, then project files.
func Detect(dir string, argv []string) Detection {
	cmd := strings.ToLower(strings.Join(argv, " "))
	pkg := readPackage(dir)

	// Explicit commands win over project metadata.
	switch {
	case strings.Contains(cmd, "artisan serve"):
		return Detection{adapterByName("laravel"), "command artisan serve"}
	case strings.Contains(cmd, "manage.py runserver"):
		return Detection{adapterByName("django"), "command manage.py runserver"}
	case strings.Contains(cmd, "uvicorn"):
		return Detection{adapterByName("uvicorn"), "command uvicorn"}
	case strings.Contains(cmd, "flask run"):
		return Detection{adapterByName("flask"), "command flask run"}
	case strings.Contains(cmd, "mix phx.server") || strings.Contains(cmd, "phoenix.server"):
		return Detection{adapterByName("phoenix"), "command mix phx.server"}
	case strings.Contains(cmd, "bin/rails") || strings.Contains(cmd, "rails server") || strings.Contains(cmd, "rails s"):
		return Detection{adapterByName("rails"), "command rails server"}
	case strings.Contains(cmd, "vite"):
		return Detection{adapterByName("vite"), "command vite"}
	case strings.Contains(cmd, "next"):
		return Detection{adapterByName("next"), "command next"}
	case strings.Contains(cmd, "nuxt"):
		return Detection{adapterByName("nuxt"), "command nuxt"}
	case strings.Contains(cmd, "astro"):
		return Detection{adapterByName("astro"), "command astro"}
	case strings.Contains(cmd, "svelte-kit"):
		return Detection{adapterByName("sveltekit"), "command svelte-kit"}
	case strings.Contains(cmd, "go run") || strings.HasPrefix(cmd, "go "):
		return Detection{adapterByName("go"), "command go"}
	}

	if pkg != nil {
		if _, ok := pkg.DevDependencies["@sveltejs/kit"]; ok {
			return Detection{adapterByName("sveltekit"), "package.json @sveltejs/kit"}
		}
		if _, ok := pkg.Dependencies["@sveltejs/kit"]; ok {
			return Detection{adapterByName("sveltekit"), "package.json @sveltejs/kit"}
		}
		if _, ok := pkg.Dependencies["astro"]; ok {
			return Detection{adapterByName("astro"), "package.json astro"}
		}
		if _, ok := pkg.Dependencies["nuxt"]; ok {
			return Detection{adapterByName("nuxt"), "package.json nuxt"}
		}
		if _, ok := pkg.Dependencies["next"]; ok {
			return Detection{adapterByName("next"), "package.json next"}
		}
		if _, ok := pkg.Dependencies["vite"]; ok {
			return Detection{adapterByName("vite"), "package.json vite"}
		}
		if _, ok := pkg.DevDependencies["vite"]; ok {
			return Detection{adapterByName("vite"), "package.json vite"}
		}
		if _, ok := pkg.Dependencies["express"]; ok {
			return Detection{adapterByName("express"), "package.json express"}
		}
	}
	if exists(dir, "manage.py") {
		return Detection{adapterByName("django"), "manage.py present"}
	}
	if exists(dir, "mix.exs") {
		return Detection{adapterByName("phoenix"), "mix.exs present"}
	}
	if exists(dir, "artisan") {
		return Detection{adapterByName("laravel"), "artisan present"}
	}
	if exists(dir, "Gemfile") && exists(dir, filepath.Join("config", "application.rb")) {
		return Detection{adapterByName("rails"), "Gemfile and config/application.rb present"}
	}
	if exists(dir, "go.mod") {
		return Detection{adapterByName("go"), "go.mod present"}
	}
	return Detection{Adapter{Name: "generic", DefaultPort: 3000, EnvOnly: true}, "no framework detected; PORT/HOST environment only"}
}

// PortArgs computes the flag injection for a command. It understands npm/pnpm
// and bun's need for a `--` separator.
func PortArgs(argv []string, det Detection, port int, override string) []string {
	host := "127.0.0.1"
	var flags []string
	if strings.TrimSpace(override) != "" {
		for _, f := range strings.Fields(override) {
			f = strings.ReplaceAll(f, "{port}", itoa(port))
			f = strings.ReplaceAll(f, "{host}", host)
			flags = append(flags, f)
		}
	} else {
		for _, f := range det.Adapter.Flags {
			f = strings.ReplaceAll(f, "{port}", itoa(port))
			f = strings.ReplaceAll(f, "{host}", host)
			flags = append(flags, f)
		}
		if det.Adapter.Positional != "" {
			flags = append(flags, strings.NewReplacer("{port}", itoa(port), "{host}", host).Replace(det.Adapter.Positional))
		}
	}
	if len(flags) == 0 {
		return nil
	}
	runner := ""
	if len(argv) > 0 {
		runner = filepath.Base(argv[0])
	}
	switch runner {
	case "npm", "pnpm", "bun", "bunx":
		return append([]string{"--"}, flags...)
	default:
		return flags
	}
}

// Env returns the environment overrides applied to the child process.
func Env(port int, host string, det Detection) []string {
	env := []string{
		"PORT=" + itoa(port),
		"HOST=127.0.0.1",
	}
	if det.Adapter.Name == "vite" || det.Adapter.Name == "sveltekit" {
		env = append(env, "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS="+host)
	}
	return env
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
