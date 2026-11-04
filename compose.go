package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// composeArgs builds `docker compose` arguments that address the project
// exactly as it was started, so commands work from any working directory.
// Without its compose files only label-based commands (logs, stop, down, …) work.
func (d *Docker) composeArgs(p ComposeProject, args ...string) []string {
	a := []string{"compose", "-p", p.Name}
	if !p.ComposeConfigAvailable() {
		return d.CLIArgs(append(a, args...)...)
	}
	if p.WorkingDir != "" {
		a = append(a, "--project-directory", p.WorkingDir)
	}
	for _, f := range p.ConfigFiles {
		a = append(a, "-f", f)
	}
	for _, f := range p.EnvFiles {
		if _, err := os.Stat(f); err == nil {
			a = append(a, "--env-file", f)
		}
	}
	return d.CLIArgs(append(a, args...)...)
}

// ComposeConfigAvailable reports whether the project's compose files still
// exist locally, which `up` and service discovery depend on.
func (p ComposeProject) ComposeConfigAvailable() bool {
	if len(p.ConfigFiles) == 0 {
		return false
	}
	for _, f := range p.ConfigFiles {
		if _, err := os.Stat(f); err != nil {
			return false
		}
	}
	return true
}

// Compose runs a non-interactive compose command and returns its combined output.
func (d *Docker) Compose(ctx context.Context, p ComposeProject, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", d.composeArgs(p, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker compose %s: %w\n%s", strings.Join(args, " "), err, lastLines(out.String(), 6))
	}
	return out.String(), nil
}

// ComposeServices lists the services declared in the project's compose files.
func (d *Docker) ComposeServices(ctx context.Context, p ComposeProject) ([]string, error) {
	if !p.ComposeConfigAvailable() {
		return nil, fmt.Errorf("compose files of %q not found", p.Name)
	}
	out, err := d.Compose(ctx, p, "config", "--services")
	if err != nil {
		return nil, err
	}
	var svcs []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			svcs = append(svcs, l)
		}
	}
	return svcs, nil
}

// ComposeLogs streams `docker compose logs` for a project or one service.
func (d *Docker) ComposeLogs(ctx context.Context, p ComposeProject, service string, follow, timestamps bool, tail string) (io.ReadCloser, error) {
	args := []string{"logs", "--no-color", "--tail", tail}
	if follow {
		args = append(args, "-f")
	}
	if timestamps {
		args = append(args, "--timestamps")
	}
	if service != "" {
		args = append(args, service)
	}
	cmd := exec.CommandContext(ctx, "docker", d.composeArgs(p, args...)...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { pw.CloseWithError(cmd.Wait()) }()
	return pr, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
