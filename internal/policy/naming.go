package policy

import (
	"regexp"
	"strings"
)

var invalidLabel = regexp.MustCompile(`[^a-z0-9-]+`)

const MaxLabel = 63

// SanitizeLabel converts an arbitrary Docker/Compose name into one DNS label.
func SanitizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "/")
	s = invalidLabel.ReplaceAllString(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "app"
	}
	if len(s) > MaxLabel {
		s = strings.Trim(s[:MaxLabel], "-")
		if s == "" {
			s = "app"
		}
	}
	return s
}

// SanitizeHost normalizes a hostname that may contain dots. Each label is
// sanitized independently and empty labels are removed.
func SanitizeHost(s string) string {
	s = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(s), "."))
	parts := strings.Split(s, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, SanitizeLabel(p))
	}
	if len(out) == 0 {
		return "app"
	}
	return strings.Join(out, ".")
}

// NormalizeResolve accepts either a bare name ("database") or a full name
// ("database.localhost") and returns a DNS-safe hostname.
func NormalizeResolve(resolve, domain string) string {
	resolve = strings.TrimSpace(strings.ToLower(resolve))
	resolve = strings.TrimSuffix(resolve, ".")
	if resolve == "" {
		return ""
	}
	if strings.Contains(resolve, ".") {
		return SanitizeHost(resolve)
	}
	return SanitizeLabel(resolve) + "." + domain
}

// DefaultHost is <service>.<project>.<domain> with an empty project omitted.
func DefaultHost(service, project, domain string) string {
	service = SanitizeLabel(service)
	project = strings.TrimSpace(project)
	if project == "" {
		return service + "." + domain
	}
	project = SanitizeLabel(project)
	if project == "default" {
		return service + "." + domain
	}
	h := service + "." + project + "." + domain
	if len([]rune(h)) > 200 {
		return SanitizeLabel(service) + "." + domain
	}
	return h
}

// CollisionHost relocates a short host under its project, e.g.
// database.localhost -> database.shop.localhost.
func CollisionHost(host, project, domain string) string {
	project = SanitizeLabel(project)
	if project == "" || project == "default" {
		project = "default"
	}
	host = SanitizeHost(host)
	base := host
	if strings.HasSuffix(host, "."+domain) {
		base = strings.TrimSuffix(host, "."+domain)
	}
	first := strings.Split(base, ".")[0]
	first = SanitizeLabel(first)
	return first + "." + project + "." + domain
}

// UniqueHost appends -2, -3 ... when even the collision host is taken.
func UniqueHost(host string, taken func(string) bool) string {
	if !taken(host) {
		return host
	}
	labels := strings.Split(host, ".")
	base := host
	suffix := ""
	if len(labels) > 2 {
		// Keep the last two labels as the domain suffix when possible.
		base = strings.Join(labels[:len(labels)-2], ".")
		suffix = "." + strings.Join(labels[len(labels)-2:], ".")
	} else if len(labels) == 2 {
		base = labels[0]
		suffix = "." + labels[1]
	}
	for i := 2; i < 1000; i++ {
		candidate := base + "-" + itoa(i) + suffix
		if !taken(candidate) {
			return candidate
		}
	}
	return host
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
