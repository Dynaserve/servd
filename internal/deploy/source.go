package deploy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// imageSource decides whether a service source is a prebuilt container image
// (e.g. "mongo:7", "redis", "ghcr.io/org/app:tag") rather than a git repo.
// A bare "owner/repo" and any http(s) URL are treated as repos.
func imageSource(source string) (string, bool) {
	s := strings.TrimSpace(source)
	if s == "" || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return "", false
	}
	// A tag or digest marks an image ("name:tag", "name@sha256:…").
	if strings.Contains(s, ":") || strings.Contains(s, "@") {
		return s, true
	}
	// "owner/repo" (exactly one slash, no tag) is a GitHub repo.
	if strings.Count(s, "/") == 1 {
		return "", false
	}
	// A single word ("redis") or a registry path ("ghcr.io/org/app") is an image.
	return s, true
}

// parseOwnerRepo extracts owner and repo from a service source such as
// "owner/repo" or "https://github.com/owner/repo(.git)".
func parseOwnerRepo(source string) (owner, repo string) {
	s := strings.TrimSpace(source)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "http://github.com/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1]
	}
	return "", ""
}

// repoURL turns a service source into a cloneable https URL. Supports
// "owner/repo", full github URLs, and any https git URL. Container image
// sources are rejected (the deploy pipeline builds from source).
func repoURL(source string) (string, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", fmt.Errorf("no source set: pick a GitHub repository first")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s, nil
	}
	// bare owner/repo
	if strings.Count(s, "/") == 1 && !strings.Contains(s, ":") {
		return "https://github.com/" + s + ".git", nil
	}
	return "", fmt.Errorf("unsupported source %q: use owner/repo or an https git URL", source)
}

func gitClone(ctx context.Context, repo, branch, token, dir string) error {
	cloneURL := repo
	// Inject the user's token for private github.com repos over https.
	if token != "" && strings.HasPrefix(repo, "https://") {
		cloneURL = "https://x-access-token:" + token + "@" + strings.TrimPrefix(repo, "https://")
	}

	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, dir)
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		safe := string(out)
		if token != "" {
			safe = strings.ReplaceAll(safe, token, "***") // never leak the token
		}
		return fmt.Errorf("%v: %s", err, lastLines(safe, 4))
	}
	return nil
}
