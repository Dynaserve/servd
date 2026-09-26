package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// ProxyManager exposes a locally-running app (e.g. a Next.js dev server) at a
// public URL by running a small reverse proxy on a dedicated port. A dedicated
// port (rather than a path prefix) is used so apps with absolute asset paths —
// like Next.js's /_next/* — work without any rewriting.
type ProxyManager struct {
	mu         sync.Mutex
	publicHost string // hostname to advertise in URLs (e.g. localhost or a LAN IP)
	portStart  int
	portEnd    int
	nextPort   int
	active     map[string]*proxyInstance // serviceID -> running proxy
}

type proxyInstance struct {
	target string
	port   int
	server *http.Server
}

// NewProxyManager builds a manager advertising publicHost, allocating ports in
// [start, end].
func NewProxyManager(publicHost string, start, end int) *ProxyManager {
	return &ProxyManager{
		publicHost: publicHost,
		portStart:  start,
		portEnd:    end,
		nextPort:   start,
		active:     map[string]*proxyInstance{},
	}
}

// URLFor returns the public URL an exposed service is reachable at.
func (m *ProxyManager) URLFor(port int) string {
	return fmt.Sprintf("http://%s:%d", m.publicHost, port)
}

// Expose starts (or restarts) a reverse proxy for serviceID pointing at target
// (e.g. "http://localhost:3000") and returns the public URL. Re-exposing an
// already-active service reuses its port.
func (m *ProxyManager) Expose(serviceID, target string) (string, error) {
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
		resp.Header.Set("X-Powered-By", brandName)
		resp.Header.Set("Server", brandName)
		resp.Header.Set("X-Region", region)
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
	return m.URLFor(port), nil
}

// Unexpose stops a service's proxy and frees its port.
func (m *ProxyManager) Unexpose(serviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst, ok := m.active[serviceID]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = inst.server.Shutdown(ctx)
		delete(m.active, serviceID)
	}
}

// allocatePortLocked returns the next free port in range. Callers hold m.mu.
func (m *ProxyManager) allocatePortLocked() (int, error) {
	span := m.portEnd - m.portStart + 1
	for i := 0; i < span; i++ {
		port := m.nextPort
		m.nextPort++
		if m.nextPort > m.portEnd {
			m.nextPort = m.portStart
		}
		if m.portInUseLocked(port) {
			continue
		}
		if ln, err := net.Listen("tcp", ":"+strconv.Itoa(port)); err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free proxy ports in %d-%d", m.portStart, m.portEnd)
}

func (m *ProxyManager) portInUseLocked(port int) bool {
	for _, inst := range m.active {
		if inst.port == port {
			return true
		}
	}
	return false
}
