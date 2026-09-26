package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"servd/platform/internal/brand"
)

// Domain mode: one front door on :80/:443 routes by hostname. Every exposed
// service gets <title>-<hash>.<AppsDomain>, customers can attach their own
// domains, and certificates are obtained and renewed automatically (ACME,
// Let's Encrypt by default).

// route is one hostname's destination.
type route struct {
	serviceID string
	proxy     *httputil.ReverseProxy
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeDomain lowercases a domain and checks it is a valid hostname
// (at least two labels, no scheme, port or wildcard).
func NormalizeDomain(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if d == "" || len(d) > 253 {
		return "", fmt.Errorf("%q is not a valid domain", d)
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%q is not a valid domain", d)
	}
	for _, l := range labels {
		if !hostLabelRe.MatchString(l) {
			return "", fmt.Errorf("%q is not a valid domain", d)
		}
	}
	if net.ParseIP(d) != nil {
		return "", fmt.Errorf("%q is an IP address, not a domain", d)
	}
	return d, nil
}

// slug turns a service title into a DNS label fragment.
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "app"
	}
	return s
}

// generatedHost names a service under the apps domain. The hash of the
// service id keeps names unique without exposing internal ids.
func (m *Manager) generatedHost(serviceID, title string, hashLen int) string {
	sum := sha256.Sum256([]byte(serviceID))
	return slug(title) + "-" + hex.EncodeToString(sum[:])[:hashLen] + "." + m.cfg.AppsDomain
}

func (m *Manager) underAppsDomain(host string) bool {
	return host == m.cfg.AppsDomain || strings.HasSuffix(host, "."+m.cfg.AppsDomain)
}

func (m *Manager) exposeDomain(r Route) (*Exposure, error) {
	target, err := url.Parse(r.Target)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid target %q: want e.g. http://10.88.0.5:3000", r.Target)
	}
	rt := &route{serviceID: r.ServiceID, proxy: m.newReverseProxy(target)}

	m.mu.Lock()
	defer m.mu.Unlock()

	// The service's own name under the apps domain: keep the one it had.
	host := ""
	if h, err := NormalizeDomain(r.Hostname); err == nil && m.underAppsDomain(h) && h != m.cfg.AppsDomain {
		if cur, taken := m.routes[h]; !taken || cur.serviceID == r.ServiceID {
			host = h
		}
	}
	for n := 6; host == ""; n += 4 {
		h := m.generatedHost(r.ServiceID, r.Title, n)
		if cur, taken := m.routes[h]; !taken || cur.serviceID == r.ServiceID || n >= 60 {
			host = h
		}
	}

	exp := &Exposure{Hostname: host, Rejected: map[string]string{}}
	hosts := []string{host}
	claimsChanged := false
	for _, raw := range r.Domains {
		d, err := NormalizeDomain(raw)
		switch {
		case err != nil:
			exp.Rejected[raw] = err.Error()
		case m.underAppsDomain(d) || d == m.cfg.APIDomain:
			exp.Rejected[d] = "reserved for the platform"
		case m.claims[d] != "" && m.claims[d] != r.ServiceID:
			exp.Rejected[d] = "already used by another service"
		default:
			if m.claims[d] == "" {
				m.claims[d] = r.ServiceID
				claimsChanged = true
			}
			hosts = append(hosts, d)
			exp.Domains = append(exp.Domains, d)
		}
	}
	if claimsChanged {
		if err := m.saveClaims(); err != nil {
			return nil, err
		}
	}

	// Swap this service's hostnames in one step.
	for _, old := range m.hosts[r.ServiceID] {
		delete(m.routes, old)
	}
	for _, h := range hosts {
		m.routes[h] = rt
	}
	m.hosts[r.ServiceID] = hosts

	scheme := "https"
	if !m.cfg.TLS {
		scheme = "http"
	}
	exp.URL = scheme + "://" + host
	return exp, nil
}

func (m *Manager) newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // apps see the domain they were called on
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			brand.Stamp(resp.Header, m.cfg.Region)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			page(w, m.cfg.Region, http.StatusBadGateway, "This app isn't responding",
				"It may be starting up or restarting. Try again in a few seconds.")
		},
	}
}

// ServeHTTP routes a request by its Host header.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	if host == m.cfg.APIDomain && m.api != nil {
		m.api.ServeHTTP(w, r)
		return
	}
	m.mu.Lock()
	rt := m.routes[host]
	m.mu.Unlock()
	if rt == nil {
		page(w, m.cfg.Region, http.StatusNotFound, "No app here",
			"Nothing is deployed at "+host+" right now.")
		return
	}
	rt.proxy.ServeHTTP(w, r)
}

// allowedHost reports whether host may receive a certificate: only names we
// actually route, so strangers pointing DNS at us can't make us request
// certificates (or burn the CA's rate limits).
func (m *Manager) allowedHost(host string) bool {
	host = strings.ToLower(host)
	if host == m.cfg.APIDomain && m.api != nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[host] != nil
}

// Serve runs the front door until ctx ends. In port mode it returns at once.
func (m *Manager) Serve(ctx context.Context) error {
	if !m.DomainMode() {
		return nil
	}
	var servers []*http.Server
	errc := make(chan error, 2)
	start := func(srv *http.Server, tlsOn bool) {
		servers = append(servers, srv)
		go func() {
			var err error
			if tlsOn {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", srv.Addr, err)
			}
		}()
	}

	if m.cfg.TLS {
		mgr := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(filepath.Join(m.cfg.StateDir, "certs")),
			HostPolicy: func(_ context.Context, host string) error {
				if m.allowedHost(host) {
					return nil
				}
				return fmt.Errorf("host %q is not served here", host)
			},
			Email: m.cfg.ACMEEmail,
		}
		if m.cfg.ACMEDirectory != "" || m.cfg.ACMEHTTPClient != nil {
			mgr.Client = &acme.Client{DirectoryURL: m.cfg.ACMEDirectory, HTTPClient: m.cfg.ACMEHTTPClient}
		}
		tlsCfg := mgr.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		// Port 80: ACME http-01 challenges, everything else redirects to HTTPS.
		start(&http.Server{Addr: m.cfg.HTTPAddr, Handler: mgr.HTTPHandler(http.HandlerFunc(m.redirect)), ReadHeaderTimeout: 10 * time.Second}, false)
		start(&http.Server{Addr: m.cfg.HTTPSAddr, Handler: m, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}, true)
		log.Printf("router: serving *.%s on %s (HTTP→HTTPS) and %s with automatic certificates", m.cfg.AppsDomain, m.cfg.HTTPAddr, m.cfg.HTTPSAddr)
	} else {
		start(&http.Server{Addr: m.cfg.HTTPAddr, Handler: m, ReadHeaderTimeout: 10 * time.Second}, false)
		log.Printf("router: serving *.%s on %s (HTTPS off)", m.cfg.AppsDomain, m.cfg.HTTPAddr)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		for _, s := range servers {
			_ = s.Close()
		}
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}

// redirect sends plain-HTTP visitors of routed hosts to HTTPS.
func (m *Manager) redirect(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	if !m.allowedHost(host) {
		page(w, m.cfg.Region, http.StatusNotFound, "No app here", "Nothing is deployed at "+host+" right now.")
		return
	}
	target := "https://" + host
	if _, port, err := net.SplitHostPort(m.cfg.HTTPSAddr); err == nil && port != "443" {
		target += ":" + port
	}
	http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

// --- custom domain claims ---
//
// A custom domain belongs to the first service that claims it, recorded on
// disk so the order services come back in after a restart can't hand it to
// someone else. Claims are released when the service is deleted.

func (m *Manager) claimsPath() string { return filepath.Join(m.cfg.StateDir, "domains.json") }

func (m *Manager) loadClaims() error {
	b, err := os.ReadFile(m.claimsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &m.claims)
}

// saveClaims writes the claims file. Callers hold m.mu.
func (m *Manager) saveClaims() error {
	b, err := json.MarshalIndent(m.claims, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.claimsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.claimsPath())
}

func hostOnly(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func page(w http.ResponseWriter, region string, status int, title, msg string) {
	brand.Stamp(w.Header(), region)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>%[1]s</title>
<body style="font:16px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0;color:#1b2420;background:#f3f5f4">
<div style="max-width:30rem;padding:2rem"><h1 style="font-size:1.4rem;margin:0 0 .5rem">%[1]s</h1><p style="margin:0;color:#56655f">%[2]s</p>
<p style="margin-top:1.5rem;font-size:.8rem;color:#8a9892">%[3]d · %[4]s</p></div>`,
		html.EscapeString(title), html.EscapeString(msg), status, brand.Name)
}
