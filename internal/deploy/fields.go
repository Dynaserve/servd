package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"servd/platform/internal/proxy"
	"servd/platform/internal/store"
)

// Helpers for reading loosely-typed fields off a stored service.

// boolOf coerces a stored field to bool (handles the JSON bool type).
func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	// service env vars are stored as [{key,value}] by the frontend.
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				k, _ := m["key"].(string)
				val, _ := m["value"].(string)
				if k != "" {
					out[k] = val
				}
			}
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Key is the platform-wide identity of a user's service. Service ids are
// only unique per user (the dashboard picks ids like "svc-1"), so everything
// shared between tenants — app sandboxes, routes, domain claims, build
// caches — is keyed by owner and id together.
func Key(user, serviceID string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + serviceID))
	return hex.EncodeToString(sum[:8])
}

// ServiceRoute describes how a service should be reachable, from its stored
// fields: its title (names its hostname), the hostname it already has, and
// any custom domains the customer attached.
func ServiceRoute(user string, svc store.Service, target string) proxy.Route {
	title, _ := svc["title"].(string)
	host, _ := svc["hostname"].(string)
	var domains []string
	if arr, ok := svc["domains"].([]any); ok {
		for _, d := range arr {
			if s, ok := d.(string); ok && s != "" {
				domains = append(domains, s)
			}
		}
	}
	return proxy.Route{ServiceID: Key(user, store.IDOf(svc)), Title: title, Hostname: host, Domains: domains, Target: target}
}
