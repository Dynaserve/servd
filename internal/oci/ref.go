// Package oci is the platform's daemonless image store. It pulls images
// straight from OCI/Docker registries (parallel, digest-verified), keeps each
// layer unpacked exactly once so every app built on it shares it, and exports
// images back to the standard OCI layout for portability.
package oci

import (
	"fmt"
	"strings"
)

// Ref is a parsed image reference such as "node:22-alpine" or
// "ghcr.io/org/app@sha256:…".
type Ref struct {
	Registry  string // e.g. "registry-1.docker.io", "gcr.io"
	Repo      string // e.g. "library/node"
	Reference string // tag or digest
}

func (r Ref) String() string {
	sep := ":"
	if strings.HasPrefix(r.Reference, "sha256:") {
		sep = "@"
	}
	return r.Registry + "/" + r.Repo + sep + r.Reference
}

// dockerHub is the registry host bare names ("node", "user/app") resolve to.
const dockerHub = "registry-1.docker.io"

// ParseRef parses an image reference, applying Docker Hub defaults.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return Ref{}, fmt.Errorf("invalid image reference %q", s)
	}
	var ref Ref
	name := s
	if i := strings.Index(name, "@"); i >= 0 {
		ref.Reference = name[i+1:]
		name = name[:i]
	} else if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		ref.Reference = name[i+1:]
		name = name[:i]
	}
	if ref.Reference == "" {
		ref.Reference = "latest"
	}
	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Registry, ref.Repo = first, rest
	} else {
		ref.Registry, ref.Repo = dockerHub, name
	}
	if ref.Registry == "docker.io" || ref.Registry == "index.docker.io" {
		ref.Registry = dockerHub
	}
	if ref.Registry == dockerHub && !strings.Contains(ref.Repo, "/") {
		ref.Repo = "library/" + ref.Repo
	}
	if ref.Repo == "" {
		return Ref{}, fmt.Errorf("invalid image reference %q", s)
	}
	return ref, nil
}
