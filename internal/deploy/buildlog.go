package deploy

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"servd/platform/internal/ids"
)

// maxLogLines caps how many log lines a service keeps (verbose build output can
// be large, so this is generous).
const maxLogLines = 800

// log appends a single log line to the service.
func (d *Deployer) log(user, id string, line map[string]any) {
	d.appendLogs(user, id, []map[string]any{line})
}

// appendLogs appends several log lines in one store write (chronological order).
func (d *Deployer) appendLogs(user, id string, lines []map[string]any) {
	if len(lines) == 0 {
		return
	}
	svc, _, err := d.store.GetService(user, id)
	if err != nil {
		return
	}
	var logs []any
	if existing, ok := svc["logs"].([]any); ok {
		logs = existing
	}
	for _, l := range lines {
		logs = append(logs, l)
	}
	if len(logs) > maxLogLines {
		logs = logs[len(logs)-maxLogLines:]
	}
	d.store.PatchService(user, id, map[string]any{"logs": logs}) //nolint:errcheck
}

// streamBuild runs a build (via `run`, which streams raw output lines to the
// callback it's given) while batching those lines into the service log every
// 700ms, so verbose output stays cheap to persist.
func (d *Deployer) streamBuild(user, id string, run func(onLine func(string)) error) error {
	var (
		mu      sync.Mutex
		pending []map[string]any
	)
	flush := func() {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		d.appendLogs(user, id, batch)
	}

	ticker := time.NewTicker(700 * time.Millisecond)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				flush()
			case <-stop:
				return
			}
		}
	}()

	err := run(func(line string) {
		mu.Lock()
		pending = append(pending, logLine(buildLevel(line), line))
		mu.Unlock()
	})

	ticker.Stop()
	close(stop)
	flush() // final flush of anything buffered
	return err
}

// buildWithLogs builds the image at dir, streaming cleaned output to the
// service log. buildEnv is exposed to the build as BuildKit secrets.
func (d *Deployer) buildWithLogs(ctx context.Context, user, id, dir, tag string, buildEnv map[string]string) error {
	return d.streamBuild(user, id, func(onLine func(string)) error {
		return d.docker.BuildStream(ctx, dir, tag, buildEnv, func(raw string) {
			if line, ok := cleanBuildLine(raw); ok {
				onLine(line)
			}
		})
	})
}

var (
	bkStep    = regexp.MustCompile(`^#\d+\s+`)           // "#10 "
	bkElapsed = regexp.MustCompile(`^\d+\.\d+\s+`)       // "6.358 "
	bkMount   = regexp.MustCompile(`^(--mount=\S+\s+)+`) // "--mount=type=cache,... "
)

// cleanBuildLine turns raw BuildKit --progress=plain output into a readable
// log line, dropping internal noise (layer resolution, cache/transfer status,
// progress spam) and keeping real command output. Returns ok=false to skip.
func cleanBuildLine(raw string) (string, bool) {
	line := bkStep.ReplaceAllString(raw, "")    // drop "#10 "
	line = bkElapsed.ReplaceAllString(line, "") // drop elapsed "6.358 "
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	lower := strings.ToLower(trimmed)

	// BuildKit internals and status lines — noise.
	for _, p := range []string{
		"done", "cached", "sha256:", "transferring", "load build", "load metadata",
		"load .dockerignore", "load remote", "resolve ", "extracting", "naming to",
		"exporting", "writing image", "preparing", "building with", "=> ", "auth ",
		"[internal]", "importing cache", "sending tarball", "docker-image://",
		"unpacking to", "view build details", "verifying explicit fetch",
	} {
		if strings.HasPrefix(lower, p) {
			return "", false
		}
	}
	// Package-manager progress spam and box-drawing chrome.
	switch {
	case strings.Contains(trimmed, "Progress: resolved"),
		strings.HasPrefix(trimmed, "+"),
		strings.HasPrefix(trimmed, "│"),
		strings.HasPrefix(trimmed, "╭"),
		strings.HasPrefix(trimmed, "╰"),
		strings.HasPrefix(lower, "downloading"):
		return "", false
	}

	// Step headers: "[stage-0 4/6] RUN <cmd>" → show only real commands as "$ cmd".
	if strings.HasPrefix(trimmed, "[") {
		if i := strings.Index(trimmed, "] "); i >= 0 {
			rest := trimmed[i+2:]
			if cmd := strings.TrimPrefix(rest, "RUN "); cmd != rest {
				cmd = bkMount.ReplaceAllString(cmd, "") // hide cache/secret mount flags
				return "$ " + cmd, true
			}
			return "", false // COPY / WORKDIR / FROM are noise
		}
		return "", false
	}
	return trimmed, true
}

// buildLevel classifies a build-output line for colouring in the logs.
func buildLevel(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "error") || strings.Contains(l, "failed") || strings.Contains(l, "npm err"):
		return "error"
	case strings.Contains(l, "warn"):
		return "warning"
	default:
		return "info"
	}
}

func logLine(level, msg string) map[string]any {
	return map[string]any{
		"id":      ids.New(),
		"at":      time.Now().UnixMilli(),
		"level":   level,
		"message": msg,
	}
}

func info(m string) map[string]any    { return logLine("info", m) }
func success(m string) map[string]any { return logLine("success", m) }
func errline(m string) map[string]any { return logLine("error", m) }
