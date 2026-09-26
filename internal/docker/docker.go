// Package docker drives the docker CLI to build, run and inspect app containers.
package docker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Client wraps the `docker` CLI. Shelling out (rather than the SDK) keeps the
// dependency surface small and matches how ops teams run these commands.
type Client struct {
	network string
}

// New returns a client whose app containers run on the given bridge network.
func New(network string) *Client { return &Client{network: network} }

// Available reports whether the docker daemon is reachable.
func (d *Client) Available(ctx context.Context) bool {
	return exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run() == nil
}

// EnsureNetwork creates the isolated bridge network apps run on, if missing.
func (d *Client) EnsureNetwork(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "network", "inspect", d.network).Run() == nil {
		return nil
	}
	_, err := d.run(ctx, "network", "create", "--driver", "bridge", d.network)
	return err
}

// Pull fetches a prebuilt image, returning the combined output.
func (d *Client) Pull(ctx context.Context, image string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
	return string(out), err
}

// ImagePort returns the first port an image EXPOSEs, falling back to a known
// default for common images (databases, web servers), else 3000.
func (d *Client) ImagePort(ctx context.Context, image string) int {
	out, err := d.run(ctx, "image", "inspect", "--format",
		"{{range $p, $_ := .Config.ExposedPorts}}{{$p}} {{end}}", image)
	if err == nil {
		for _, tok := range strings.Fields(out) {
			portStr := strings.SplitN(tok, "/", 2)[0]
			if n, err := strconv.Atoi(portStr); err == nil && n > 0 {
				return n
			}
		}
	}
	return KnownImagePort(image)
}

// BuildStream builds an image and streams every line of build output (BuildKit
// progress, npm/next output, …) to onLine as it happens. secrets are exposed
// to the build as BuildKit secrets (id = key), so their values never land in
// image layers or history.
func (d *Client) BuildStream(ctx context.Context, dir, tag string, secrets map[string]string, onLine func(string)) error {
	args := []string{"build", "--progress=plain", "-t", tag}
	// BuildKit gives faster, parallel builds and cache mounts; plain progress
	// streams cleanly line-by-line.
	env := append(os.Environ(), "DOCKER_BUILDKIT=1")
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		// Hand values to the CLI under private names so a user variable such
		// as PATH or DOCKER_HOST can't change how the docker CLI itself runs.
		name := fmt.Sprintf("SERVD_BUILD_SECRET_%d", i)
		args = append(args, "--secret", "id="+k+",env="+name)
		env = append(env, name+"="+secrets[k])
	}
	cmd := exec.CommandContext(ctx, "docker", append(args, dir)...)
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // BuildKit writes progress to stderr; merge them
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	for sc.Scan() {
		onLine(sc.Text())
	}
	return cmd.Wait()
}

// RunSpec describes a hardened container to launch.
type RunSpec struct {
	Name          string
	Image         string
	HostPort      int // published on 127.0.0.1 only
	ContainerPort int
	Env           map[string]string
	MemoryMB      int
	CPUs          string // e.g. "0.5"
	PidsLimit     int
}

// RunSecure starts a detached, isolated container and returns its id.
//
// Isolation applied:
//   - dedicated bridge network (no host networking)
//   - memory / CPU / PID limits
//   - all Linux capabilities dropped
//   - no-new-privileges (blocks setuid escalation)
//   - published only to 127.0.0.1 (never 0.0.0.0); the platform proxy fronts it
//   - no host bind mounts, not privileged
func (d *Client) RunSecure(ctx context.Context, s RunSpec) (string, error) {
	args := []string{
		"run", "-d",
		"--name", s.Name,
		"--network", d.network,
		"--memory", strconv.Itoa(s.MemoryMB) + "m",
		"--memory-swap", strconv.Itoa(s.MemoryMB) + "m", // disallow swap growth
		"--cpus", s.CPUs,
		"--pids-limit", strconv.Itoa(s.PidsLimit),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", s.HostPort, s.ContainerPort),
		"-e", "PORT=" + strconv.Itoa(s.ContainerPort),
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, s.Image)

	out, err := d.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Stop force-removes a container by id or name.
func (d *Client) Stop(ctx context.Context, id string) error {
	_, err := d.run(ctx, "rm", "-f", id)
	return err
}

// Logs returns the last `tail` lines of a container's logs.
func (d *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	return d.run(ctx, "logs", "--tail", strconv.Itoa(tail), id)
}

func (d *Client) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// KnownImagePort maps common images to the port they listen on, used when the
// image declares no EXPOSE.
func KnownImagePort(image string) int {
	base := image
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "mongo", "mongodb":
		return 27017
	case "postgres", "postgresql":
		return 5432
	case "mysql", "mariadb":
		return 3306
	case "redis":
		return 6379
	case "rabbitmq":
		return 5672
	case "nginx", "httpd", "caddy":
		return 80
	default:
		return 3000
	}
}
