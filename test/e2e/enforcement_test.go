//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func authzCall(t *testing.T, socket, path string, body any) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	resp, err := unixClient(socket).Post("http://plugin"+path, "application/json", rdr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnforcementPlugin(t *testing.T) {
	h := NewHarness(t)
	h.MustCLI("enforce", "on")

	act := authzCall(t, h.AuthzSocket, "/Plugin.Activate", nil)
	impl, _ := act["Implements"].([]any)
	if len(impl) == 0 || impl[0] != "authz" {
		t.Fatalf("activate: %+v", act)
	}
	body := map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string]any{"80/tcp": []any{map[string]any{"HostIp": "", "HostPort": "8080"}}},
		},
	}
	reqBody, _ := json.Marshal(body)
	req := map[string]any{
		"User": "", "RequestMethod": "POST",
		"RequestUri":     "/v1.45/containers/create?name=denied-direct",
		"RequestBody":    reqBody,
		"RequestHeaders": map[string]string{},
	}
	resp := authzCall(t, h.AuthzSocket, "/AuthZPlugin.AuthZReq", req)
	if allow, _ := resp["Allow"].(bool); allow {
		t.Fatalf("direct binding allowed: %+v", resp)
	}
	if msg, _ := resp["Msg"].(string); !strings.Contains(msg, "gw allow") {
		t.Fatalf("deny message: %+v", resp)
	}

	// A signed create from the proxy is allowed.
	key, err := os.ReadFile(filepath.Join(h.StateDir, "enforce.key"))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(reqBody)
	req["RequestHeaders"] = map[string]string{"X-Prox-Signature": hex.EncodeToString(mac.Sum(nil))}
	resp = authzCall(t, h.AuthzSocket, "/AuthZPlugin.AuthZReq", req)
	if allow, _ := resp["Allow"].(bool); !allow {
		t.Fatalf("signed request denied: %+v", resp)
	}

	// A container created through the proxy must succeed while enforcement is on.
	h.EnsureImage("alpine:3.22")
	name := "proxe2e-authz-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--rm", "--name", name, "alpine:3.22", "sleep", "5")

	// Publish rule allows direct binding.
	h.AddRule(map[string]any{"name": "nginx:alpine", "action": "publish"})
	req["RequestHeaders"] = map[string]string{}
	resp = authzCall(t, h.AuthzSocket, "/AuthZPlugin.AuthZReq", req)
	if allow, _ := resp["Allow"].(bool); !allow {
		t.Fatalf("publish rule denied: %+v", resp)
	}

	h.MustCLI("enforce", "off")
	if _, err := os.Stat(h.AuthzSocket); err == nil {
		t.Fatal("authz socket still exists after enforce off")
	}
}

// TestEnforcementRawSocket is opt-in: Docker must already be configured with
// `"authorization-plugins": ["prox-authz"]` and the daemon must be able to
// create /run/docker/plugins/prox-authz.sock. The CI workflow provides this.
func TestEnforcementRawSocket(t *testing.T) {
	if os.Getenv("PROX_E2E_AUTHZ") != "1" {
		t.Skip("set PROX_E2E_AUTHZ=1 with dockerd authorization-plugins=[prox-authz] to run")
	}
	if err := os.MkdirAll("/run/docker/plugins", 0o755); err != nil {
		t.Skipf("cannot create /run/docker/plugins: %v", err)
	}
	t.Setenv("PROX_AUTHZ_SOCKET", "/run/docker/plugins/prox-authz.sock")
	h := NewHarness(t)
	h.EnsureImage("alpine:3.22")
	h.MustCLI("enforce", "on")

	// Raw socket: must be rejected.
	_, err := h.DockerRaw("run", "--rm", "-p", fmt.Sprintf("%d:80", freePort(t)), "alpine:3.22", "true")
	if err == nil {
		t.Fatal("raw socket create with a host port succeeded under enforcement")
	}
	if !strings.Contains(err.Error(), "authorization denied") && !strings.Contains(err.Error(), "prox enforcement") {
		t.Fatalf("unexpected raw error: %v", err)
	}

	// Proxy socket: rewritten and allowed.
	name := "proxe2e-authz-raw-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--rm", "--name", name, "alpine:3.22", "sleep", "5")
}
