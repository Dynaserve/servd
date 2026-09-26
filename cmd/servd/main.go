// Command servd is the Servd control-plane API. It persists services to a JSON
// file (or PostgreSQL when DATABASE_URL is set) and serves the REST API the
// frontend consumes. Docker is optional: without it, deploys only record a
// marker.
//
// Config via environment:
//
//	LISTEN_ADDR  address to bind (default ":8080" — all interfaces, LAN-reachable)
//	DATA_FILE    path to the JSON store (default "./data/store.json")
//
// See README.md for the full list.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"servd/platform/internal/api"
	"servd/platform/internal/deploy"
	"servd/platform/internal/docker"
	"servd/platform/internal/github"
	"servd/platform/internal/proxy"
	"servd/platform/internal/store"
)

func main() {
	addr := env("LISTEN_ADDR", ":8080")
	publicHost := env("PUBLIC_HOST", "localhost")
	region := env("REGION", "AU") // advertised in the X-Region response header

	st, err := openStore()
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	px := proxy.NewManager(publicHost, region, 9000, 9100)
	restoreProxies(st, px)

	// Auth + at-rest encryption config.
	sessionSecret := os.Getenv("SESSION_SECRET")
	if sessionSecret == "" {
		log.Printf("SESSION_SECRET unset: DEV AUTH (trusts X-User header) — not for production")
	} else {
		log.Printf("session auth enabled (verifies the frontend session cookie)")
	}
	var encKey []byte
	if k := os.Getenv("ENCRYPTION_KEY"); len(k) == 32 {
		encKey = []byte(k)
	} else if k != "" {
		log.Printf("ENCRYPTION_KEY must be 32 bytes (got %d): GitHub token storage disabled", len(k))
	}

	// Optional GitHub App: mints installation tokens for private-repo clones.
	githubApp, err := github.New()
	if err != nil {
		log.Printf("github app config error: %v", err)
	} else if githubApp != nil {
		log.Printf("github app enabled (private-repo installs)")
	}

	// Deploy engine: real clone→build→isolated-container→URL pipeline, enabled
	// only when the docker daemon is reachable.
	var deployer *deploy.Deployer
	dc := docker.New(env("APPS_NETWORK", "servd-apps"))
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if dc.Available(dctx) {
		if err := dc.EnsureNetwork(dctx); err != nil {
			log.Printf("docker network setup failed: %v", err)
		} else {
			deployer = deploy.New(st, dc, px, encKey, githubApp)
			deployer.Prewarm() // pre-pull base images so first builds start warm
			builder := "Dockerfile buildpack"
			if deployer.UsesRailpack() {
				builder = "Railpack (auto-detect + BuildKit cache)"
			}
			log.Printf("deploy engine enabled; builder: %s; pre-warming base images", builder)
		}
	} else {
		log.Printf("docker not available: deploys will only record a marker (no real build/run)")
	}
	dcancel()

	srv := api.New(api.Config{
		Store:         st,
		Proxy:         px,
		Deployer:      deployer,
		SessionSecret: sessionSecret,
		EncKey:        encKey,
		Region:        region,
	})
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("servd-platform listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// openStore selects the persistence backend: PostgreSQL when DATABASE_URL is
// set, otherwise the zero-dependency JSON file store.
func openStore() (store.Storer, error) {
	if url := firstEnv("DATABASE_URL", "POSTGRES_URL"); url != "" {
		log.Printf("using PostgreSQL store")
		return store.NewPostgresStore(url)
	}
	dataFile := env("DATA_FILE", "./data/store.json")
	log.Printf("using file store (%s)", dataFile)
	return store.NewFileStore(dataFile)
}

// firstEnv returns the first of keys that is set and non-empty.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// restoreProxies re-exposes services that were exposed before a restart, so
// their public URLs keep working.
func restoreProxies(st store.Storer, px *proxy.Manager) {
	all, err := st.AllServices()
	if err != nil {
		log.Printf("restore proxies: list services failed: %v", err)
		return
	}
	for _, svc := range all {
		exposed, _ := svc["exposed"].(bool)
		target, _ := svc["proxyTarget"].(string)
		if exposed && target != "" {
			if _, err := px.Expose(store.IDOf(svc), target); err != nil {
				log.Printf("restore proxy for %s failed: %v", store.IDOf(svc), err)
			}
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
