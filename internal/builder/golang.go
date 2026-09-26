package builder

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var goDirectiveRe = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`)

func detectGo(dir, mounts string) (*Plan, error) {
	gomod := readFile(dir, "go.mod")
	version, from := goVersion(gomod)
	pkg, err := goMainPackage(dir, gomod)
	if err != nil {
		return nil, err
	}
	details := []string{"Go " + version + from}
	if pkg != "." {
		details = append(details, pkg)
	}
	return &Plan{Stack: "go", Port: 8080, Details: details, Dockerfile: fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM golang:%s-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build%s \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app %s

FROM %s
COPY --from=build /out/app /app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app"]
`, version, mounts, pkg, goRunImage)}, nil
}

// goVersion returns the toolchain to build with: the go.mod directive when it
// is newer than the default (the images set GOTOOLCHAIN=local), otherwise the
// default, since newer Go builds older modules fine.
func goVersion(gomod string) (string, string) {
	m := goDirectiveRe.FindStringSubmatch(gomod)
	if m == nil {
		return defaultGo, ""
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	dm := goDirectiveRe.FindStringSubmatch("go " + defaultGo)
	dmajor, _ := strconv.Atoi(dm[1])
	dminor, _ := strconv.Atoi(dm[2])
	if major > dmajor || (major == dmajor && minor > dminor) {
		return m[1] + "." + m[2], " from go.mod"
	}
	return defaultGo, ""
}

// goMainPackage finds the package to build: the module root if it is a main
// package, else the single main package under cmd/ (or the one named after
// the module when there are several).
func goMainPackage(dir, gomod string) (string, error) {
	if isMainPackage(dir) {
		return ".", nil
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cmd"))
	var cands []string
	for _, e := range entries {
		if e.IsDir() && isMainPackage(filepath.Join(dir, "cmd", e.Name())) {
			cands = append(cands, e.Name())
		}
	}
	switch len(cands) {
	case 0:
		return "", fmt.Errorf("no main package found in the repo root or cmd/*")
	case 1:
		return "./cmd/" + cands[0], nil
	}
	if m := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindStringSubmatch(gomod); m != nil {
		for _, c := range cands {
			if c == path.Base(m[1]) {
				return "./cmd/" + c, nil
			}
		}
	}
	return "", fmt.Errorf("several main packages under cmd/ (%s): add a Dockerfile to pick one", strings.Join(cands, ", "))
}

var packageMainRe = regexp.MustCompile(`(?m)^package\s+main\b`)

func isMainPackage(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && packageMainRe.Match(b) {
			return true
		}
	}
	return false
}
