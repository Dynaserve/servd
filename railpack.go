package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// railpack wraps the Railpack CLI (github.com/railwayapp/railpack), a
// BuildKit-native builder with strong language/framework auto-detection and
// fine-grained caching. It is optional: when the binary or a BuildKit daemon
// isn't available, the deploy engine falls back to the Dockerfile buildpack.
type railpack struct {
	bin          string // path to the railpack binary
	buildkitHost string // e.g. docker-container://buildkit
}

// newRailpack locates the binary and ensures a BuildKit daemon is running.
// Returns nil when Railpack can't be used.
func newRailpack(docker *dockerctl) *railpack {
	bin := findRailpack()
	if bin == "" {
		return nil
	}
	host, err := ensureBuildkit()
	if err != nil {
		return nil
	}
	return &railpack{bin: bin, buildkitHost: host}
}

func findRailpack() string {
	if b := os.Getenv("RAILPACK_BIN"); b != "" {
		if _, err := os.Stat(b); err == nil {
			return b
		}
	}
	if p, err := exec.LookPath("railpack"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".local", "bin", "railpack")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// ensureBuildkit starts the moby/buildkit daemon container if it isn't running.
func ensureBuildkit() (string, error) {
	running := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "buildkit").Run() == nil
	if !running {
		_ = exec.Command("docker", "rm", "-f", "buildkit").Run()
		if err := exec.Command("docker", "run", "--rm", "--privileged", "-d",
			"--name", "buildkit", "moby/buildkit").Run(); err != nil {
			return "", err
		}
		time.Sleep(2 * time.Second) // let it come up
	}
	return "docker-container://buildkit", nil
}

// build runs `railpack build`, loading the image into Docker and streaming
// output to onLine. cacheKey scopes the build cache (use one per service for
// fast redeploys); env are build-time variables (e.g. NEXT_PUBLIC_*).
func (r *railpack) build(ctx context.Context, dir, tag, cacheKey string, env map[string]string, onLine func(string)) error {
	args := []string{"build", dir, "--name", tag, "--progress", "plain"}
	if cacheKey != "" {
		args = append(args, "--cache-key", cacheKey)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Env = append(os.Environ(), "BUILDKIT_HOST="+r.buildkitHost)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		onLine(sc.Text())
	}
	return cmd.Wait()
}
