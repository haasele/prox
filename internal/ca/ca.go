package ca

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const Nickname = "prox local CA"

type Result struct {
	Applied  bool
	Commands []string
	Details  []string
}

type Status struct {
	System  bool
	NSS     bool
	Firefox bool
	Details []string
}

// InstallSystem installs the root certificate into the distribution trust
// store. Without root it returns the exact commands instead of failing.
func InstallSystem(pem []byte) Result {
	var res Result
	path, update := distroAnchor()
	if path == "" {
		res.Details = append(res.Details, "no known distribution trust-store path found")
		return res
	}
	res.Commands = append(res.Commands,
		fmt.Sprintf("%s ca show | sudo tee %s > /dev/null", selfPath(), path),
		"sudo "+update,
	)
	if os.Geteuid() != 0 {
		res.Details = append(res.Details, "not running as root; system trust store unchanged")
		return res
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		res.Details = append(res.Details, "mkdir: "+err.Error())
		return res
	}
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		res.Details = append(res.Details, "write: "+err.Error())
		return res
	}
	if err := run(update); err != nil {
		res.Details = append(res.Details, update+": "+err.Error())
		return res
	}
	res.Applied = true
	res.Details = append(res.Details, "installed "+path)
	return res
}

func UninstallSystem() Result {
	var res Result
	path, update := distroAnchor()
	if path == "" {
		return res
	}
	res.Commands = append(res.Commands, "sudo rm -f "+path, "sudo "+update)
	if os.Geteuid() != 0 {
		return res
	}
	if err := os.Remove(path); err == nil {
		_ = run(update)
		res.Applied = true
		res.Details = append(res.Details, "removed "+path)
	}
	return res
}

func distroAnchor() (path, update string) {
	candidates := []struct{ dir, file, update string }{
		{"/usr/local/share/ca-certificates", "prox-local-ca.crt", "update-ca-certificates"},
		{"/etc/pki/ca-trust/source/anchors", "prox-local-ca.crt", "update-ca-trust extract"},
		{"/etc/ca-certificates/trust-source/anchors", "prox-local-ca.crt", "trust extract-compat"},
		{"/etc/pki/trust/anchors", "prox-local-ca.crt", "update-ca-certificates"},
	}
	for _, c := range candidates {
		if st, err := os.Stat(c.dir); err == nil && st.IsDir() {
			return filepath.Join(c.dir, c.file), c.update
		}
	}
	// Alpine, Void and other distributions use update-ca-certificates but may
	// not ship the Debian anchor directory by default. InstallSystem creates it.
	if _, err := exec.LookPath("update-ca-certificates"); err == nil {
		return "/usr/local/share/ca-certificates/prox-local-ca.crt", "update-ca-certificates"
	}
	return "", ""
}

// InstallNSS adds the CA to the user's Chrome/Chromium NSS database and each
// Firefox profile. This works without root.
func InstallNSS(pem []byte) Result {
	var res Result
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		res.Commands = append(res.Commands,
			"sudo apt install libnss3-tools   # Debian/Ubuntu",
			"sudo dnf install nss-tools      # Fedora",
			"sudo pacman -S nss              # Arch",
			"sudo zypper install mozilla-nss-tools  # openSUSE",
			"sudo apk add nss-tools          # Alpine",
		)
		res.Details = append(res.Details, "certutil not found; install NSS tools to trust the CA in Chrome/Firefox")
		return res
	}
	tmp, err := os.CreateTemp("", "prox-ca-*.pem")
	if err != nil {
		res.Details = append(res.Details, err.Error())
		return res
	}
	defer os.Remove(tmp.Name())
	_, _ = tmp.Write(pem)
	_ = tmp.Close()

	dbs := []string{}
	if u, err := user.Current(); err == nil {
		nss := filepath.Join(u.HomeDir, ".pki", "nssdb")
		if err := ensureNSSDB(certutil, nss); err == nil {
			dbs = append(dbs, "sql:"+nss)
		}
		if matches, _ := filepath.Glob(filepath.Join(u.HomeDir, ".mozilla", "firefox", "*", "cert9.db")); len(matches) > 0 {
			for _, m := range matches {
				dbs = append(dbs, "sql:"+filepath.Dir(m))
			}
		}
	}
	if len(dbs) == 0 {
		res.Details = append(res.Details, "no NSS databases found")
		return res
	}
	for _, db := range dbs {
		cmd := exec.Command(certutil, "-d", db, "-A", "-t", "C,,", "-n", Nickname, "-i", tmp.Name())
		if out, err := cmd.CombinedOutput(); err != nil {
			res.Commands = append(res.Commands, fmt.Sprintf("certutil -d %s -A -t 'C,,' -n %q -i <ca.pem>", db, Nickname))
			res.Details = append(res.Details, fmt.Sprintf("%s: %v %s", db, err, strings.TrimSpace(string(out))))
			continue
		}
		res.Applied = true
		res.Details = append(res.Details, "installed in "+db)
	}
	return res
}

func UninstallNSS() Result {
	var res Result
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		return res
	}
	u, err := user.Current()
	if err != nil {
		return res
	}
	var dbs []string
	nss := filepath.Join(u.HomeDir, ".pki", "nssdb")
	if _, err := os.Stat(nss); err == nil {
		dbs = append(dbs, "sql:"+nss)
	}
	if matches, _ := filepath.Glob(filepath.Join(u.HomeDir, ".mozilla", "firefox", "*", "cert9.db")); len(matches) > 0 {
		for _, m := range matches {
			dbs = append(dbs, "sql:"+filepath.Dir(m))
		}
	}
	for _, db := range dbs {
		if out, err := exec.Command(certutil, "-d", db, "-D", "-n", Nickname).CombinedOutput(); err != nil {
			res.Details = append(res.Details, fmt.Sprintf("%s: %v %s", db, err, strings.TrimSpace(string(out))))
			continue
		}
		res.Applied = true
		res.Details = append(res.Details, "removed from "+db)
	}
	return res
}

func ensureNSSDB(certutil, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "cert9.db")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command(certutil, "-d", "sql:"+dir, "-N", "--empty-password")
	return cmd.Run()
}

// Check reports whether the root is present in system and NSS trust stores.
func Check(pem []byte) Status {
	var st Status
	want := certBody(pem)
	bundles := []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/cert.pem",
		"/etc/ca-certificates/extracted/tls-ca-bundle.pem",
	}
	for _, b := range bundles {
		data, err := os.ReadFile(b)
		if err != nil {
			continue
		}
		if want != "" && bytes.Contains(data, []byte(want)) {
			st.System = true
			st.Details = append(st.Details, "found in "+b)
			break
		}
	}
	if anchor, _ := distroAnchor(); anchor != "" {
		if data, err := os.ReadFile(anchor); err == nil && bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(pem)) {
			st.System = true
			st.Details = append(st.Details, "found in "+anchor)
		}
	}
	certutil, err := exec.LookPath("certutil")
	if err == nil {
		u, _ := user.Current()
		if u != nil {
			if err := os.MkdirAll(filepath.Join(u.HomeDir, ".pki"), 0o700); err == nil {
				// Best-effort: listing fails loudly for missing DBs.
			}
			if hasNSSNick(certutil, "sql:"+filepath.Join(u.HomeDir, ".pki", "nssdb")) {
				st.NSS = true
				st.Details = append(st.Details, "found in Chrome NSS database")
			}
			if matches, _ := filepath.Glob(filepath.Join(u.HomeDir, ".mozilla", "firefox", "*", "cert9.db")); len(matches) > 0 {
				for _, m := range matches {
					if hasNSSNick(certutil, "sql:"+filepath.Dir(m)) {
						st.Firefox = true
						st.Details = append(st.Details, "found in "+filepath.Dir(m))
					}
				}
			}
		}
	}
	return st
}

func hasNSSNick(certutil, db string) bool {
	out, err := exec.Command(certutil, "-d", db, "-L", "-n", Nickname).CombinedOutput()
	return err == nil && !bytes.Contains(out, []byte("not found"))
}

func certBody(pem []byte) string {
	lines := strings.Split(string(pem), "\n")
	var body []string
	in := false
	for _, l := range lines {
		if strings.Contains(l, "BEGIN CERTIFICATE") {
			in = true
			continue
		}
		if strings.Contains(l, "END CERTIFICATE") {
			in = false
			continue
		}
		if in {
			body = append(body, strings.TrimSpace(l))
		}
	}
	return strings.Join(body, "")
}

func selfPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "gw"
}

func run(command string) error {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60e9)
	defer cancel()
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
