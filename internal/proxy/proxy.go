// Package proxy puts apps on the internet. Two modes:
//
//   - domain mode (AppsDomain set): one front door on :80/:443 routes by
//     hostname, with automatic HTTPS certificates (see router.go);
//   - port mode (default, for local use): each app gets its own port.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"servd/platform/internal/brand"
)

// Config configures the Manager.
type Config struct {
	Region string // advertised in the X-Region header

	// Port mode: each app on its own port of PublicHost, in [PortStart, PortEnd].
	PublicHost         string
	PortStart, PortEnd int

	// Domain mode, enabled by AppsDomain (e.g. "dynaserve.app" with DNS
	// *.dynaserve.app pointing here).
	AppsDomain string
	APIDomain  string // hostname that serves the platform API, e.g. "api.dynaserve.app"
	HTTPAddr   string // default ":80"
	HTTPSAddr  string // default ":443"
	TLS        bool   // automatic HTTPS via ACME
	StateDir   string // certificates and domain claims
	ACMEEmail  string
	// ACMEDirectory overrides the CA (default Let's Encrypt production); use
	// Let's Encrypt staging while testing.
	ACMEDirectory  string
	ACMEHTTPClient *http.Client // for tests against a private CA
}

// Route asks for a service to be reachable.
type Route struct {
	ServiceID string
	Title     string   // names the generated hostname
	Hostname  string   // hostname the service had before, kept if still valid
	Domains   []string // the customer's own domains
	Target    string   // where the app listens, e.g. http://10.88.0.5:3000
}

// Exposure is where a service ended up.
type Exposure struct {
	URL      string            // primary public URL
	Hostname string            // generated hostname (domain mode)
	Domains  []string          // custom domains now routed
	Rejected map[string]string // custom domain -> why it wasn't routed
}

// Manager exposes apps at public URLs.
type Manager struct {
	cfg Config
	mu  sync.Mutex

	// port mode
	nextPort int
	active   map[string]*proxyInstance // serviceID -> running proxy

	// domain mode
	routes map[string]*route   // hostname -> route
	hosts  map[string][]string // serviceID -> its hostnames
	claims map[string]string   // custom domain -> owning serviceID (persisted)
	api    http.Handler
}

type proxyInstance struct {
	target string
	port   int
	server *http.Server
}

// NewManager builds a Manager. In domain mode it loads the domain claims
// from StateDir.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":80"
	}
	if cfg.HTTPSAddr == "" {
		cfg.HTTPSAddr = ":443"
	}
	m := &Manager{
		cfg:      cfg,
		nextPort: cfg.PortStart,
		active:   map[string]*proxyInstance{},
		routes:   map[string]*route{},
		hosts:    map[string][]string{},
		claims:   map[string]string{},
	}
	if cfg.AppsDomain != "" {
		d, err := NormalizeDomain(cfg.AppsDomain)
		if err != nil {
			return nil, fmt.Errorf("apps domain: %w", err)
		}
		m.cfg.AppsDomain = d
		if cfg.APIDomain != "" {
			if m.cfg.APIDomain, err = NormalizeDomain(cfg.APIDomain); err != nil {
				return nil, fmt.Errorf("API domain: %w", err)
			}
		}
		if cfg.StateDir == "" {
			return nil, errors.New("domain mode needs a state directory")
		}
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return nil, err
		}
		if err := m.loadClaims(); err != nil {
			return nil, fmt.Errorf("load domain claims: %w", err)
		}
	}
	return m, nil
}

// DomainMode reports whether apps are served by hostname on :80/:443.
func (m *Manager) DomainMode() bool { return m.cfg.AppsDomain != "" }

// SetAPIHandler serves the platform API on the API domain.
func (m *Manager) SetAPIHandler(h http.Handler) {
	m.mu.Lock()
	m.api = h
	m.mu.Unlock()
}

// APIURL is the public URL of the platform API in domain mode ("" otherwise).
func (m *Manager) APIURL() string {
	if !m.DomainMode() || m.cfg.APIDomain == "" {
		return ""
	}
	if m.cfg.TLS {
		return "https://" + m.cfg.APIDomain
	}
	return "http://" + m.cfg.APIDomain
}

// Expose makes a service reachable (or updates where it points) and returns
// its public URL. Re-exposing keeps the service's address.
func (m *Manager) Expose(r Route) (*Exposure, error) {
	if m.DomainMode() {
		return m.exposeDomain(r)
	}
	u, err := m.exposePort(r.ServiceID, r.Target)
	if err != nil {
		return nil, err
	}
	return &Exposure{URL: u}, nil
}

// Release forgets a deleted service's custom domains, freeing them for others.
func (m *Manager) Release(serviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for d, owner := range m.claims {
		if owner == serviceID {
			delete(m.claims, d)
			changed = true
		}
	}
	if changed {
		if err := m.saveClaims(); err != nil {
			log.Printf("router: save domain claims: %v", err)
		}
	}
}

// exposePort starts (or restarts) a per-port reverse proxy for serviceID.
func (m *Manager) exposePort(serviceID, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid target %q: want e.g. http://localhost:3000", target)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reuse the port if this service is already exposed; just repoint it.
	port := 0
	if existing, ok := m.active[serviceID]; ok {
		port = existing.port
		_ = existing.server.Close()
	} else {
		p, err := m.allocatePortLocked()
		if err != nil {
			return "", err
		}
		port = p
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	// Preserve the upstream's expectations and keep the app happy behind us.
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		r.Host = u.Host // let the app see its own host
	}
	// Override Server / X-Powered-By so the app's framework (Express, Next.js, …)
	// isn't leaked through the platform; everything served here is "Dynaserve".
	proxy.ModifyResponse = func(resp *http.Response) error {
		brand.Stamp(resp.Header, m.cfg.Region)
		return nil
	}

	server := &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: proxy,
	}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return "", fmt.Errorf("listen :%d: %w", port, err)
	}
	go func() { _ = server.Serve(ln) }()

	m.active[serviceID] = &proxyInstance{target: target, port: port, server: server}
	return fmt.Sprintf("http://%s:%d", m.cfg.PublicHost, port), nil
}

// Unexpose takes a service offline (its custom domains stay claimed).
func (m *Manager) Unexpose(serviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.hosts[serviceID] {
		delete(m.routes, h)
	}
	delete(m.hosts, serviceID)
	if inst, ok := m.active[serviceID]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = inst.server.Shutdown(ctx)
		delete(m.active, serviceID)
	}
}

// allocatePortLocked returns the next free port in range. Callers hold m.mu.
func (m *Manager) allocatePortLocked() (int, error) {
	span := m.cfg.PortEnd - m.cfg.PortStart + 1
	for i := 0; i < span; i++ {
		port := m.nextPort
		m.nextPort++
		if m.nextPort > m.cfg.PortEnd {
			m.nextPort = m.cfg.PortStart
		}
		if m.portInUseLocked(port) {
			continue
		}
		if ln, err := net.Listen("tcp", ":"+strconv.Itoa(port)); err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free proxy ports in %d-%d", m.cfg.PortStart, m.cfg.PortEnd)
}

func (m *Manager) portInUseLocked(port int) bool {
	for _, inst := range m.active {
		if inst.port == port {
			return true
		}
	}
	return false
}
