package builder

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func detectPython(dir, mounts string) (*Plan, error) {
	version, from := pythonVersion(dir)
	deps := strings.ToLower(readFile(dir, "requirements.txt") + "\n" + readFile(dir, "pyproject.toml"))

	install := "pip install ."
	if exists(dir, "requirements.txt") {
		install = "pip install -r requirements.txt"
	}
	start, how, err := pythonStart(dir, deps)
	if err != nil {
		return nil, err
	}
	return &Plan{Stack: "python", Port: 8000, Details: []string{"Python " + version + from, how}, Dockerfile: fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM python:%s-slim
WORKDIR /app
ENV PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY . .
RUN --mount=type=cache,target=/root/.cache/pip%s %s
ENV PORT=8000
EXPOSE 8000
%s
`, version, mounts, install, shellCmd(start))}, nil
}

// pythonStart works out the start command, in order of preference: a Procfile
// web process, Django, an ASGI app under uvicorn, a WSGI app under gunicorn,
// then plain `python <file>`. It also returns a short label for the log.
func pythonStart(dir, deps string) (cmd, how string, err error) {
	for _, line := range strings.Split(readFile(dir, "Procfile"), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "web:"); ok && strings.TrimSpace(rest) != "" {
			return strings.TrimSpace(rest), "Procfile", nil
		}
	}
	hasGunicorn := strings.Contains(deps, "gunicorn")

	if exists(dir, "manage.py") {
		if wsgi, _ := filepath.Glob(filepath.Join(dir, "*", "wsgi.py")); len(wsgi) > 0 && hasGunicorn {
			mod := filepath.Base(filepath.Dir(wsgi[0])) + ".wsgi"
			return "gunicorn " + mod + ":application --bind 0.0.0.0:$PORT", "Django + gunicorn", nil
		}
		return "python manage.py runserver 0.0.0.0:$PORT", "Django", nil
	}

	for _, f := range []string{"main.py", "app.py", "server.py"} {
		if !exists(dir, f) {
			continue
		}
		mod := strings.TrimSuffix(f, ".py")
		switch {
		case strings.Contains(deps, "uvicorn") || strings.Contains(deps, "fastapi[standard]"):
			return "uvicorn " + mod + ":app --host 0.0.0.0 --port $PORT", "uvicorn", nil
		case hasGunicorn:
			return "gunicorn " + mod + ":app --bind 0.0.0.0:$PORT", "gunicorn", nil
		default:
			return "python " + f, f, nil
		}
	}
	return "", "", fmt.Errorf("don't know how to start this Python app: add a Procfile with a web: command, or a main.py/app.py")
}

var pyVersionRe = regexp.MustCompile(`3\.(\d+)`)

// pythonVersion reads the major.minor from .python-version, runtime.txt or
// pyproject's requires-python. Open-ended ranges (">=3.9") resolve to at
// least the default.
func pythonVersion(dir string) (string, string) {
	for _, f := range []string{".python-version", "runtime.txt"} {
		if m := pyVersionRe.FindString(readFile(dir, f)); m != "" {
			return m, " from " + f
		}
	}
	for _, line := range strings.Split(readFile(dir, "pyproject.toml"), "\n") {
		spec, ok := strings.CutPrefix(strings.TrimSpace(line), "requires-python")
		if !ok {
			continue
		}
		spec = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(spec), "=")), `"'`)
		m := pyVersionRe.FindStringSubmatch(spec)
		if m == nil {
			break
		}
		if strings.HasPrefix(spec, ">") {
			minor, _ := strconv.Atoi(m[1])
			dminor, _ := strconv.Atoi(pyVersionRe.FindStringSubmatch(defaultPython)[1])
			if minor <= dminor {
				return defaultPython, ""
			}
		}
		return m[0], " from pyproject.toml"
	}
	return defaultPython, ""
}
