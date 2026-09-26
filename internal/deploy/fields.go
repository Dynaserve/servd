package deploy

import (
	"strings"
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
