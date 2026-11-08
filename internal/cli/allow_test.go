package cli

import (
	"strings"
	"testing"
)

func TestNormalizePortAliases(t *testing.T) {
	cases := []struct {
		name    string
		in      portAliases
		source  int
		target  int
		wantErr string
	}{
		{name: "source", in: portAliases{source: 3000, sourceSet: true}, source: 3000},
		{name: "container alias", in: portAliases{container: 3000, containerSet: true}, source: 3000},
		{name: "target", in: portAliases{target: 5432, targetSet: true}, target: 5432},
		{name: "gateway alias", in: portAliases{gateway: 5432, gatewaySet: true}, target: 5432},
		{name: "both source spellings", in: portAliases{source: 1, container: 2, sourceSet: true, containerSet: true}, wantErr: "aliases"},
		{name: "both target spellings", in: portAliases{target: 1, gateway: 2, targetSet: true, gatewaySet: true}, wantErr: "aliases"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, tg, err := normalizePortAliases(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s != c.source || tg != c.target {
				t.Fatalf("got source=%d target=%d, want %d/%d", s, tg, c.source, c.target)
			}
		})
	}
}
