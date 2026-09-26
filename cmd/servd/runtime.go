package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"time"

	"servd/platform/internal/deploy"
	"servd/platform/internal/docker"
	"servd/platform/internal/engine"
)

// openRuntime picks where images are built and apps run, from RUNTIME:
//
//	native  the built-in daemonless engine (needs root and runc or crun)
//	docker  a Docker daemon
//	auto    native when possible, else docker (the default)
//
// It returns nil (deploys only record a marker) when neither is usable.
// The native engine's supervisor runs until ctx ends.
func openRuntime(ctx context.Context) deploy.Runtime {
	mode := env("RUNTIME", "auto")
	if mode == "native" || mode == "auto" {
		rt, err := openNative(ctx)
		if err == nil {
			return rt
		}
		if mode == "native" {
			log.Fatalf("native runtime: %v", err)
		}
		log.Printf("native runtime unavailable (%v); trying docker", err)
	}
	if mode == "docker" || mode == "auto" {
		dc := docker.New(env("APPS_NETWORK", "servd-apps"))
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if !dc.Available(dctx) {
			log.Printf("docker not available: deploys will only record a marker (no real build/run)")
			return nil
		}
		if err := dc.EnsureNetwork(dctx); err != nil {
			log.Printf("docker network setup failed: %v", err)
			return nil
		}
		log.Printf("runtime: docker")
		return deploy.NewDockerRuntime(dc)
	}
	log.Fatalf("unknown RUNTIME %q (want native, docker or auto)", mode)
	return nil
}

func openNative(ctx context.Context) (deploy.Runtime, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("needs root")
	}
	bin := os.Getenv("OCI_RUNTIME")
	if bin == "" {
		if _, err := exec.LookPath("runc"); err != nil {
			if _, err := exec.LookPath("crun"); err != nil {
				return nil, fmt.Errorf("no runc or crun on PATH")
			}
		}
	}
	shift, err := strconv.Atoi(env("ENGINE_ID_SHIFT", "100000"))
	if err != nil {
		return nil, fmt.Errorf("ENGINE_ID_SHIFT: %w", err)
	}
	eng, err := engine.New(engine.Config{
		Root:       env("ENGINE_ROOT", "/var/lib/servd"),
		Bridge:     env("ENGINE_BRIDGE", "servd0"),
		Subnet:     env("ENGINE_SUBNET", "10.88.0.0/16"),
		IDShift:    shift,
		Mirror:     os.Getenv("REGISTRY_MIRROR"),
		RuntimeBin: bin,
	})
	if err != nil {
		return nil, err
	}
	go eng.Supervise(ctx)
	log.Printf("runtime: native engine (daemonless builds, sandboxed apps)")
	return deploy.NewNativeRuntime(eng), nil
}
