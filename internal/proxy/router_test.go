package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func newDomainManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := NewManager(Config{AppsDomain: "Dynaserve.App", APIDomain: "api.dynaserve.app", StateDir: dir, Region: "AU"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// backend is an app that echoes what it received.
func backend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Express")
		io.WriteString(w, name+" host="+r.Host+" proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(s.Close)
	return s
}

func get(m *Manager, host string) (int, string, http.Header) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://"+host+"/path", nil)
	m.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestDomainRouting(t *testing.T) {
	dir := t.TempDir()
	m := newDomainManager(t, dir)
	a, b := backend(t, "app-a"), backend(t, "app-b")

	expA, err := m.Expose(Route{ServiceID: "key-a", Title: "My API Server!", Target: a.URL, Domains: []string{"WWW.Example.com."}})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^my-api-server-[0-9a-f]{6}\.dynaserve\.app$`).MatchString(expA.Hostname) {
		t.Errorf("hostname = %q", expA.Hostname)
	}
	if expA.URL != "http://"+expA.Hostname || len(expA.Domains) != 1 || expA.Domains[0] != "www.example.com" {
		t.Errorf("exposure = %+v", expA)
	}

	// Routed by Host, original host preserved, brand headers replace the app's.
	code, body, hdr := get(m, expA.Hostname)
	if code != 200 || !strings.HasPrefix(body, "app-a host="+expA.Hostname) || hdr.Get("Server") != "Dynaserve" {
		t.Errorf("GET %s = %d %q %v", expA.Hostname, code, body, hdr)
	}
	if _, body, _ := get(m, "www.example.com:443"); !strings.HasPrefix(body, "app-a host=www.example.com") {
		t.Errorf("custom domain: %q", body)
	}

	// Another service can't take the domain, or a platform name.
	expB, err := m.Expose(Route{ServiceID: "key-b", Title: "b", Target: b.URL,
		Domains: []string{"www.example.com", "evil.dynaserve.app", "api.dynaserve.app", "not a domain"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(expB.Domains) != 0 || len(expB.Rejected) != 4 {
		t.Errorf("service b should get no custom domains: %+v", expB)
	}
	if _, body, _ := get(m, "www.example.com"); !strings.HasPrefix(body, "app-a") {
		t.Error("domain hijacked")
	}

	// Redeploy to a new target keeps the same hostname.
	a2 := backend(t, "app-a-v2")
	again, _ := m.Expose(Route{ServiceID: "key-a", Title: "Renamed", Hostname: expA.Hostname, Target: a2.URL, Domains: []string{"www.example.com"}})
	if again.Hostname != expA.Hostname {
		t.Errorf("hostname changed on redeploy: %s -> %s", expA.Hostname, again.Hostname)
	}
	if _, body, _ := get(m, expA.Hostname); !strings.HasPrefix(body, "app-a-v2") {
		t.Errorf("redeploy not routed: %q", body)
	}
	// A service can't take over another's generated hostname.
	steal, _ := m.Expose(Route{ServiceID: "key-b", Title: "b", Hostname: expA.Hostname, Target: b.URL})
	if steal.Hostname == expA.Hostname {
		t.Error("service b took service a's hostname")
	}

	// Unknown hosts get a 404 page; certificate policy follows the routes.
	if code, _, _ := get(m, "nothing.dynaserve.app"); code != 404 {
		t.Errorf("unknown host = %d", code)
	}
	if !m.allowedHost(expA.Hostname) || !m.allowedHost("www.example.com") || m.allowedHost("random.dynaserve.app") {
		t.Error("host policy wrong")
	}

	// The API domain serves the platform API.
	m.SetAPIHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "api") }))
	if _, body, _ := get(m, "api.dynaserve.app"); body != "api" {
		t.Errorf("api domain = %q", body)
	}

	// Claims survive a restart; releasing frees the domain.
	m2 := newDomainManager(t, dir)
	if exp, _ := m2.Expose(Route{ServiceID: "key-b", Title: "b", Target: b.URL, Domains: []string{"www.example.com"}}); len(exp.Domains) != 0 {
		t.Error("claim lost across restart")
	}
	m2.Release("key-a")
	if exp, _ := m2.Expose(Route{ServiceID: "key-b", Title: "b", Target: b.URL, Domains: []string{"www.example.com"}}); len(exp.Domains) != 1 {
		t.Errorf("released domain not claimable: %+v", exp)
	}

	// Taking a service offline removes all its hostnames.
	m.Unexpose("key-a")
	if code, _, _ := get(m, expA.Hostname); code != 404 {
		t.Errorf("after unexpose: %d", code)
	}
}

func TestDownAppShowsPage(t *testing.T) {
	m := newDomainManager(t, t.TempDir())
	exp, _ := m.Expose(Route{ServiceID: "k", Title: "down", Target: "http://127.0.0.1:1"})
	code, body, _ := get(m, exp.Hostname)
	if code != http.StatusBadGateway || !strings.Contains(body, "isn&#39;t responding") {
		t.Errorf("down app = %d %q", code, body)
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{"WWW.Example.COM.": "www.example.com", " shop.co.uk ": "shop.co.uk"} {
		if got, err := NormalizeDomain(in); err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "http://x.com", "*.x.com", "x.com:443", "-a.com", "1.2.3.4", "a..com"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Errorf("NormalizeDomain(%q) should fail", bad)
		}
	}
}
