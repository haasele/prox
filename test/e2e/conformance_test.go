//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerWith(hostEnv, stdin string, args ...string) (string, string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+hostEnv)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func TestProxyConformance(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("alpine:3.22")
	name := "proxe2e-conf-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--name", name, "alpine:3.22", "sh", "-c", "echo line1; sleep 0.4; echo line2; sleep 600")

	proxyHost := "unix://" + h.ProxySocket
	rawHost := "unix://" + dockerSocket()

	// exec -i with stdin, compared with the raw socket.
	proxyOut, proxyErr, err := dockerWith(proxyHost, "exec-stdin\n", "exec", "-i", name, "cat")
	if err != nil {
		t.Fatalf("exec via proxy: %v %s", err, proxyErr)
	}
	rawOut, _, err := dockerWith(rawHost, "exec-stdin\n", "exec", "-i", name, "cat")
	if err != nil || proxyOut != rawOut {
		t.Fatalf("exec mismatch: proxy=%q raw=%q err=%v", proxyOut, rawOut, err)
	}

	// exec -it (requires a TTY; script(1) supplies one).
	if _, err := exec.LookPath("script"); err == nil {
		script := fmt.Sprintf("DOCKER_HOST=%s docker exec -it %s sh -c 'echo tty-ok'", proxyHost, name)
		out, err := exec.Command("script", "-qec", script, "/dev/null").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "tty-ok") {
			t.Fatalf("exec -it: %v %q", err, out)
		}
	}

	// logs -f streams both lines.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logsCmd := exec.CommandContext(ctx, "docker", "logs", "-f", name)
	logsCmd.Env = append(os.Environ(), "DOCKER_HOST="+proxyHost)
	pipe, err := logsCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	logsCmd.Stderr = logsCmd.Stdout
	if err := logsCmd.Start(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	sc := bufio.NewScanner(pipe)
	deadline := time.After(15 * time.Second)
	done := make(chan struct{})
	go func() {
		for sc.Scan() {
			logs.WriteString(sc.Text() + "\n")
			if strings.Contains(logs.String(), "line2") {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatalf("logs -f did not stream: %q", logs.String())
	}
	_ = logsCmd.Process.Kill()
	_ = logsCmd.Wait()
	if !strings.Contains(logs.String(), "line1") || !strings.Contains(logs.String(), "line2") {
		t.Fatalf("logs = %q", logs.String())
	}

	// cp from the container.
	containerPath := "/tmp/prox-cp-" + randSuffix()
	h.MustDocker("exec", name, "sh", "-c", "echo copied-file > "+containerPath)
	cpOut, cpErr, err := dockerWith(proxyHost, "", "cp", name+":"+containerPath, "-")
	if err != nil {
		t.Fatalf("cp: %v %s", err, cpErr)
	}
	if !strings.Contains(cpOut, "copied-file") {
		t.Fatalf("cp output missing content (tar bytes %d)", len(cpOut))
	}

	// attach: streaming upgrade. The container keeps writing so attach does
	// not depend on Docker replaying history.
	att := "proxe2e-attach-" + randSuffix()
	h.MustDocker("run", "-d", "--name", att, "alpine:3.22", "sh", "-c", "while true; do echo attach-ok; sleep 0.2; done")
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", att) })
	attCmd := exec.Command("docker", "attach", att)
	attCmd.Env = append(os.Environ(), "DOCKER_HOST="+proxyHost)
	attPipe, err := attCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	attCmd.Stderr = attCmd.Stdout
	if err := attCmd.Start(); err != nil {
		t.Fatal(err)
	}
	attDone := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(attPipe)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "attach-ok") {
				attDone <- sc.Text()
				return
			}
		}
		attDone <- ""
	}()
	select {
	case line := <-attDone:
		if line == "" {
			t.Fatal("attach produced no matching output")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach timed out")
	}
	_ = attCmd.Process.Kill()
	_ = attCmd.Wait()

	// build: tar upload + streaming response.
	buildDir := t.TempDir()
	writeFile(t, filepath.Join(buildDir, "Dockerfile"), "FROM alpine:3.22\nCOPY hello.txt /hello.txt\nCMD [\"cat\", \"/hello.txt\"]\n")
	writeFile(t, filepath.Join(buildDir, "hello.txt"), "built-through-proxy\n")
	tag := "proxe2e-build:" + randSuffix()
	out, err := h.Docker("build", "-t", tag, buildDir)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = h.DockerRaw("rmi", "-f", tag) })
	runOut := h.MustDockerRaw("run", "--rm", tag)
	if strings.TrimSpace(runOut) != "built-through-proxy" {
		t.Fatalf("built image output = %q", runOut)
	}

	// events: stream through the proxy while a container is created.
	evName := "proxe2e-events-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", evName) })
	evCmd := exec.Command("docker", "events", "--filter", "type=container", "--filter", "event=create", "--format", "{{.Action}}")
	evCmd.Env = append(os.Environ(), "DOCKER_HOST="+proxyHost)
	evPipe, err := evCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := evCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = evCmd.Process.Kill(); _ = evCmd.Wait() }()
	h.MustDocker("create", "--name", evName, "alpine:3.22", "true")
	gotEvent := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(evPipe)
		for sc.Scan() {
			gotEvent <- sc.Text()
			return
		}
		gotEvent <- ""
	}()
	select {
	case ev := <-gotEvent:
		if !strings.Contains(ev, "create") {
			t.Fatalf("events = %q", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("events stream produced nothing")
	}
}
