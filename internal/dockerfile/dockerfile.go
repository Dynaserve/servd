// Package dockerfile parses the subset of the Dockerfile language the
// platform's native builder executes: everything the in-house detector
// generates, plus what typical hand-written app Dockerfiles use —
// multi-stage FROM … AS, RUN (shell/exec form, --mount=type=cache|secret),
// COPY (--from, --chown, --chmod, heredocs), ENV, ARG, WORKDIR, USER, CMD,
// ENTRYPOINT, EXPOSE, and ignorable metadata (LABEL, HEALTHCHECK, …).
//
// Anything else is a parse error, so callers can fall back to a full builder.
package dockerfile

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Stage is one FROM section.
type Stage struct {
	Name  string // from "AS name", lowercased; may be ""
	From  string // image reference or earlier stage name (after ARG expansion)
	Instr []Instr
}

// Instr is one instruction. Only the fields relevant to Op are set.
type Instr struct {
	Op   string // RUN, COPY, ENV, ARG, WORKDIR, USER, CMD, ENTRYPOINT, EXPOSE
	Line int    // 1-based source line, for errors
	Raw  string // the instruction as written (for logs and cache keys)

	// RUN / CMD / ENTRYPOINT
	Args  []string // exec form, or ["/bin/sh","-c",cmd] for shell form
	Shell bool     // written in shell form

	// RUN --mount
	Caches  []string // cache mount targets
	Secrets []Secret

	// COPY
	Src     []string
	Dst     string
	From    string // --from stage/image
	Chown   string
	Chmod   string
	Heredoc *Heredoc

	// ENV / ARG
	Key, Value string
	HasValue   bool
	Pairs      [][2]string

	// WORKDIR / USER / EXPOSE: Value
}

// Secret is a RUN --mount=type=secret exposed as an environment variable.
type Secret struct{ ID, Env string }

// Heredoc is inline file content for COPY <<EOF dest.
type Heredoc struct{ Body string }

// ignored instructions don't affect the image we run.
var ignored = map[string]bool{"LABEL": true, "MAINTAINER": true, "HEALTHCHECK": true, "STOPSIGNAL": true, "VOLUME": true, "ONBUILD": false}

var heredocRe = regexp.MustCompile(`^<<(-?)(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)$`)

// Parse parses Dockerfile text. buildArgs override ARG defaults.
func Parse(text string, buildArgs map[string]string) ([]Stage, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var stages []Stage
	globalArgs := map[string]string{}
	var cur *Stage
	var vars map[string]string // ARG + ENV values visible in the current stage

	for i := 0; i < len(lines); i++ {
		lineNo := i + 1
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Join continuation lines, skipping comment lines inside them.
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			if strings.HasPrefix(next, "#") {
				line = strings.TrimSuffix(line, "\\") + "\\"
				continue
			}
			line = strings.TrimSuffix(line, "\\") + " " + next
		}
		op, rest, _ := strings.Cut(line, " ")
		op = strings.ToUpper(op)
		rest = strings.TrimSpace(rest)
		in := Instr{Op: op, Line: lineNo, Raw: line}
		fail := func(format string, a ...any) error {
			return fmt.Errorf("Dockerfile line %d: %s", lineNo, fmt.Sprintf(format, a...))
		}

		if op == "FROM" {
			flags, args := splitFlags(rest)
			for _, f := range flags {
				if !strings.HasPrefix(f, "--platform=") {
					return nil, fail("unsupported FROM flag %s", f)
				}
			}
			if len(args) != 1 && !(len(args) == 3 && strings.EqualFold(args[1], "AS")) {
				return nil, fail("expected FROM image [AS name]")
			}
			st := Stage{From: expand(args[0], globalArgs)}
			if len(args) == 3 {
				st.Name = strings.ToLower(args[2])
			}
			stages = append(stages, st)
			cur = &stages[len(stages)-1]
			vars = map[string]string{}
			continue
		}
		if op == "ARG" && cur == nil {
			k, v, has := strings.Cut(rest, "=")
			if bv, ok := buildArgs[k]; ok {
				v, has = bv, true
			}
			if has {
				globalArgs[k] = unquote(v)
			} else {
				globalArgs[k] = ""
			}
			continue
		}
		if cur == nil {
			return nil, fail("%s before FROM", op)
		}
		if ignored[op] {
			continue
		}

		switch op {
		case "RUN":
			flags, cmd := cutFlags(rest)
			for _, f := range flags {
				if !strings.HasPrefix(f, "--mount=") {
					return nil, fail("unsupported RUN flag %s", f)
				}
				m := parseKV(strings.TrimPrefix(f, "--mount="))
				switch m["type"] {
				case "cache":
					target := firstNonEmpty(m["target"], m["dst"], m["destination"])
					if target == "" {
						return nil, fail("cache mount needs a target")
					}
					in.Caches = append(in.Caches, target)
				case "secret":
					id := m["id"]
					env := m["env"]
					if id == "" || env == "" {
						return nil, fail("only env secret mounts (id=…,env=…) are supported")
					}
					in.Secrets = append(in.Secrets, Secret{ID: id, Env: env})
				default:
					return nil, fail("unsupported mount type %q", m["type"])
				}
			}
			if strings.HasPrefix(cmd, "<<") {
				return nil, fail("RUN heredocs are not supported")
			}
			in.Args, in.Shell = commandForm(cmd)
			if len(in.Args) == 0 {
				return nil, fail("empty RUN")
			}
		case "CMD", "ENTRYPOINT":
			in.Args, in.Shell = commandForm(rest)
		case "COPY", "ADD":
			flags, args := splitFlags(rest)
			for _, f := range flags {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "--from":
					in.From = strings.ToLower(expand(v, vars))
				case "--chown":
					in.Chown = expand(v, vars)
				case "--chmod":
					in.Chmod = v
				case "--link":
				default:
					return nil, fail("unsupported %s flag %s", op, k)
				}
			}
			if len(args) < 2 {
				return nil, fail("%s needs a source and a destination", op)
			}
			if m := heredocRe.FindStringSubmatch(args[0]); m != nil {
				if len(args) != 2 {
					return nil, fail("heredoc COPY takes exactly one destination")
				}
				var body []string
				terminated := false
				for i+1 < len(lines) {
					i++
					l := lines[i]
					if m[1] == "-" {
						l = strings.TrimLeft(l, "\t")
					}
					if l == m[3] {
						terminated = true
						break
					}
					body = append(body, l)
				}
				if !terminated {
					return nil, fail("unterminated heredoc %s", m[3])
				}
				text := strings.Join(body, "\n") + "\n"
				if m[2] == "" { // unquoted delimiter: expand variables
					text = expand(text, vars)
				}
				in.Heredoc = &Heredoc{Body: text}
				in.Dst = expand(args[1], vars)
				in.Op = "COPY"
				break
			}
			for _, s := range args[:len(args)-1] {
				if op == "ADD" && (strings.Contains(s, "://") || isArchive(s)) {
					return nil, fail("ADD of URLs and archives is not supported; use COPY")
				}
				in.Src = append(in.Src, expand(s, vars))
			}
			in.Dst = expand(args[len(args)-1], vars)
			in.Op = "COPY"
		case "ENV":
			pairs, err := parseEnv(rest, vars)
			if err != nil {
				return nil, fail("%v", err)
			}
			in.Pairs = pairs
			for _, p := range pairs {
				vars[p[0]] = p[1]
			}
		case "ARG":
			k, v, has := strings.Cut(rest, "=")
			if gv, ok := globalArgs[k]; ok && !has {
				v, has = gv, true
			}
			if bv, ok := buildArgs[k]; ok {
				v, has = bv, true
			}
			in.Key, in.Value, in.HasValue = k, expand(unquote(v), vars), has
			vars[k] = in.Value
		case "WORKDIR", "USER":
			in.Value = expand(unquote(rest), vars)
		case "EXPOSE":
			in.Value = expand(rest, vars)
		case "SHELL", "ONBUILD":
			return nil, fail("%s is not supported by the native builder", op)
		default:
			return nil, fail("unknown instruction %s", op)
		}
		cur.Instr = append(cur.Instr, in)
	}
	if len(stages) == 0 {
		return nil, fmt.Errorf("Dockerfile has no FROM")
	}
	return stages, nil
}

// commandForm parses exec form (JSON array) or shell form.
func commandForm(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		var arr []string
		if json.Unmarshal([]byte(s), &arr) == nil {
			return arr, false
		}
	}
	if s == "" {
		return nil, true
	}
	return []string{"/bin/sh", "-c", s}, true
}

// cutFlags splits leading --flags from the rest of a line.
func cutFlags(s string) ([]string, string) {
	var flags []string
	for strings.HasPrefix(s, "--") {
		f, rest, _ := strings.Cut(s, " ")
		flags = append(flags, f)
		s = strings.TrimSpace(rest)
	}
	return flags, s
}

// splitFlags splits a line into leading --flags and whitespace-separated
// (or JSON-array) arguments.
func splitFlags(s string) ([]string, []string) {
	flags, rest := cutFlags(s)
	if strings.HasPrefix(rest, "[") {
		var arr []string
		if json.Unmarshal([]byte(rest), &arr) == nil {
			return flags, arr
		}
	}
	return flags, strings.Fields(rest)
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(part, "=")
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// parseEnv handles `ENV a=1 b="two words"` and legacy `ENV key value`.
func parseEnv(s string, vars map[string]string) ([][2]string, error) {
	first, rest, _ := strings.Cut(s, " ")
	if !strings.Contains(first, "=") {
		if first == "" {
			return nil, fmt.Errorf("ENV needs a value")
		}
		return [][2]string{{first, expand(strings.TrimSpace(rest), vars)}}, nil
	}
	var out [][2]string
	for _, tok := range shellWords(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad ENV pair %q", tok)
		}
		out = append(out, [2]string{k, expand(v, vars)})
	}
	return out, nil
}

// shellWords splits on unquoted whitespace, removing quotes.
func shellWords(s string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			b.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, b.String())
				b.Reset()
				inWord = false
			}
		default:
			b.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, b.String())
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expand substitutes $VAR, ${VAR} and ${VAR:-default}.
func expand(s string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		sm := varRe.FindStringSubmatch(m)
		name := sm[1] + sm[4]
		if v, ok := vars[name]; ok && v != "" {
			return v
		}
		return sm[3]
	})
}

func isArchive(s string) bool {
	for _, ext := range []string{".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tar.xz"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ReadIgnore loads .dockerignore patterns from a build context.
func ReadIgnore(contextDir string) []string {
	b, err := os.ReadFile(contextDir + "/.dockerignore")
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, strings.Trim(l, "/"))
		}
	}
	return out
}
