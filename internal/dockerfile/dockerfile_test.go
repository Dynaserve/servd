package dockerfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseGeneratedStyle(t *testing.T) {
	src := "# syntax=docker/dockerfile:1\n" +
		"FROM node:22-alpine AS build\n" +
		"WORKDIR /app\n" +
		"ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1\n" +
		"COPY package.json package-lock.json ./\n" +
		"RUN --mount=type=cache,target=/root/.npm --mount=type=secret,id=API,env=API npm ci\n" +
		"COPY . .\n" +
		"RUN --mount=type=cache,target=/go/pkg/mod \\\n" +
		"    # a comment inside a continuation\n" +
		"    npm run build\n" +
		"\n" +
		"FROM nginx:alpine\n" +
		"COPY --from=build /app/dist /usr/share/nginx/html\n" +
		"COPY <<'EOF' /etc/nginx/conf.d/default.conf\n" +
		"server { root $ROOT; }\n" +
		"EOF\n" +
		"EXPOSE 8080\n" +
		"CMD [\"sh\",\"-c\",\"exec nginx\"]\n"
	stages, err := Parse(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 || stages[0].Name != "build" || stages[1].From != "nginx:alpine" {
		t.Fatalf("stages = %+v", stages)
	}
	b := stages[0].Instr
	if got := b[1].Pairs; len(got) != 2 || got[1] != [2]string{"NEXT_TELEMETRY_DISABLED", "1"} {
		t.Errorf("ENV pairs = %v", got)
	}
	if !reflect.DeepEqual(b[2].Src, []string{"package.json", "package-lock.json"}) || b[2].Dst != "./" {
		t.Errorf("COPY = %+v", b[2])
	}
	run := b[3]
	if !reflect.DeepEqual(run.Caches, []string{"/root/.npm"}) || run.Secrets[0] != (Secret{"API", "API"}) ||
		!reflect.DeepEqual(run.Args, []string{"/bin/sh", "-c", "npm ci"}) {
		t.Errorf("RUN = %+v", run)
	}
	if got := b[5].Args[2]; got != "npm run build" {
		t.Errorf("continuation RUN = %q", got)
	}
	f := stages[1].Instr
	if f[0].From != "build" || f[0].Src[0] != "/app/dist" {
		t.Errorf("COPY --from = %+v", f[0])
	}
	if f[1].Heredoc == nil || f[1].Heredoc.Body != "server { root $ROOT; }\n" {
		t.Errorf("quoted heredoc must not expand: %+v", f[1].Heredoc)
	}
	if !reflect.DeepEqual(f[3].Args, []string{"sh", "-c", "exec nginx"}) || f[3].Shell {
		t.Errorf("CMD = %+v", f[3])
	}
}

func TestParseArgsAndEnvExpansion(t *testing.T) {
	src := `ARG NODE=20
FROM node:${NODE}-alpine
ARG NODE
ARG MODE=dev
ENV APP_HOME=/srv/app LEGACY_UNUSED="a b"
ENV OTHER value with spaces
WORKDIR $APP_HOME/${MODE}
USER node:node
LABEL x=y
HEALTHCHECK CMD true
CMD npm start
`
	stages, err := Parse(src, map[string]string{"MODE": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	st := stages[0]
	if st.From != "node:20-alpine" {
		t.Errorf("FROM = %q", st.From)
	}
	var wd, user string
	var cmd Instr
	for _, in := range st.Instr {
		switch in.Op {
		case "WORKDIR":
			wd = in.Value
		case "USER":
			user = in.Value
		case "CMD":
			cmd = in
		case "ENV":
			if in.Pairs[0][0] == "OTHER" && in.Pairs[0][1] != "value with spaces" {
				t.Errorf("legacy ENV = %v", in.Pairs)
			}
			if in.Pairs[0][0] == "APP_HOME" && in.Pairs[1][1] != "a b" {
				t.Errorf("quoted ENV = %v", in.Pairs)
			}
		}
	}
	if wd != "/srv/app/prod" || user != "node:node" || !cmd.Shell {
		t.Errorf("wd=%q user=%q cmd=%+v", wd, user, cmd)
	}
}

func TestParseRejectsUnsupported(t *testing.T) {
	for _, src := range []string{
		"RUN echo hi",
		"FROM a\nADD https://example.com/x /x",
		"FROM a\nSHELL [\"bash\",\"-c\"]",
		"FROM a\nRUN --mount=type=ssh ls",
		"FROM a\nRUN --network=none ls",
		"FROM a\nCOPY <<EOF /x\nno end",
		"FROM a\nFOO bar",
	} {
		if _, err := Parse(src, nil); err == nil {
			t.Errorf("expected error for %q", strings.SplitN(src, "\n", 2)[len(strings.SplitN(src, "\n", 2))-1])
		}
	}
}
