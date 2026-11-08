package daemon

import (
	"context"
	"fmt"

	"github.com/haasele/prox/internal/api"
)

func (d *Daemon) Enforce(ctx context.Context, on bool) (api.EnforcementStatus, error) {
	st := api.EnforcementStatus{Socket: d.Authz.Socket}
	if on {
		if err := d.Reg.SetMeta("enforcement", "on"); err != nil {
			return st, err
		}
		st.Enabled = true
		if err := d.Authz.Start(); err != nil {
			// Do not leave a phantom "enabled" state behind when Docker may be
			// configured to fail closed against a plugin that never started.
			_ = d.Reg.SetMeta("enforcement", "off")
			st.Enabled = false
			st.Error = err.Error()
			st.Instructions = enforceInstructions()
			d.Reg.AddEvent("error", "enforcement", "enforcement plugin failed to start: "+err.Error(), nil)
			return st, err
		}
		st.Running = true
		st.Instructions = enforceInstructions()
		d.Reg.AddEvent("info", "enforcement", "enforcement enabled", map[string]any{"socket": d.Authz.Socket})
		return st, nil
	}
	if err := d.Reg.SetMeta("enforcement", "off"); err != nil {
		return st, err
	}
	if err := d.Authz.Stop(ctx); err != nil {
		st.Error = err.Error()
		return st, err
	}
	d.Reg.AddEvent("info", "enforcement", "enforcement disabled", nil)
	return st, nil
}

func enforceInstructions() []string {
	return []string{
		fmt.Sprintf("add \"authorization-plugins\": [\"prox-authz\"] to /etc/docker/daemon.json"),
		"sudo systemctl restart docker",
		"plugin socket: /run/docker/plugins/prox-authz.sock",
		"for full lock-down remove non-admin users from the docker group and rebuild images via prox",
	}
}
