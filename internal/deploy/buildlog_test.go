package deploy

import "testing"

func TestCleanBuildLine(t *testing.T) {
	for raw, want := range map[string]string{
		"#11 [build 5/6] RUN --mount=type=cache,target=/root/.npm --mount=type=secret,id=A,env=A npm ci": "$ npm ci",
		"#12 [stage-0 2/3] COPY . .":  "",
		"#11 6.576 added 21 packages": "added 21 packages",
		"#5 DONE 0.1s":                "",
	} {
		got, ok := cleanBuildLine(raw)
		if got != want || ok != (want != "") {
			t.Errorf("cleanBuildLine(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}
