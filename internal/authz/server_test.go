package authz

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/profiles"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "authz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set, err := profiles.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	eng := policy.New(reg, set, "localhost")
	return New(filepath.Join(t.TempDir(), "authz.sock"), []byte("secret-key"), eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func createBody(port string) []byte {
	b, _ := json.Marshal(map[string]any{
		"Image":  "nginx:latest",
		"Labels": map[string]any{"com.docker.compose.project": "shop", "com.docker.compose.service": "web"},
		"HostConfig": map[string]any{
			"PortBindings": map[string]any{
				port + "/tcp": []any{map[string]any{"HostIp": "", "HostPort": port}},
			},
		},
	})
	return b
}

func signed(key, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestAuthZEvaluation(t *testing.T) {
	s := testServer(t)
	body := createBody("3000")
	base := Request{RequestMethod: "POST", RequestURI: "/v1.45/containers/create?name=shop-web-1", RequestBody: body}

	resp := s.evaluate(base)
	if resp.Allow {
		t.Fatal("unsigned direct binding must be denied")
	}
	if !strings.Contains(resp.Msg, "gw allow") || !strings.Contains(resp.Msg, "prox enforcement") {
		t.Fatalf("message = %q", resp.Msg)
	}

	base.RequestHeaders = map[string]string{"X-Prox-Signature": signed(s.Key, body)}
	if resp := s.evaluate(base); !resp.Allow {
		t.Fatalf("signed request denied: %+v", resp)
	}

	base.RequestHeaders = map[string]string{"X-Prox-Signature": strings.Repeat("0", 64)}
	if resp := s.evaluate(base); resp.Allow {
		t.Fatal("bad signature allowed")
	}

	// A publish rule permits direct binding.
	if _, err := s.Policy.AddRule(policy.RuleRequest{Name: "web", Project: "shop", Action: "publish"}); err != nil {
		t.Fatal(err)
	}
	base.RequestHeaders = nil
	if resp := s.evaluate(base); !resp.Allow {
		t.Fatalf("publish rule denied: %+v", resp)
	}

	// Non-create and portless requests pass through.
	if resp := s.evaluate(Request{RequestMethod: "GET", RequestURI: "/v1.45/containers/json"}); !resp.Allow {
		t.Fatal("GET denied")
	}
	noPorts, _ := json.Marshal(map[string]any{"Image": "alpine", "HostConfig": map[string]any{}})
	if resp := s.evaluate(Request{RequestMethod: "POST", RequestURI: "/v1.45/containers/create", RequestBody: noPorts}); !resp.Allow {
		t.Fatal("portless create denied")
	}
	hostNet, _ := json.Marshal(map[string]any{"Image": "alpine", "HostConfig": map[string]any{"NetworkMode": "host"}})
	if resp := s.evaluate(Request{RequestMethod: "POST", RequestURI: "/v1.45/containers/create", RequestBody: hostNet}); resp.Allow {
		t.Fatal("raw host-network create should be denied")
	}
}

func TestActivate(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/Plugin.Activate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"authz"`) {
		t.Fatalf("activate response = %s", b)
	}
}
