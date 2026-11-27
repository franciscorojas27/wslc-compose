package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHighFix1ProfilesExplicitServicesAndDotenv(t *testing.T) {
	composeYAML := `
name: profapp
services:
  web:
    image: nginx:alpine
  prom:
    image: prom/prometheus
    profiles:
      - metrics
  debug:
    image: alpine:latest
    profiles:
      - debug
    depends_on:
      debug-db:
        condition: service_started
  debug-db:
    image: postgres:16-alpine
    profiles:
      - storage
    depends_on:
      debug-init:
        condition: service_completed_successfully
  debug-init:
    image: busybox:latest
    profiles:
      - init
  unused:
    image: redis:alpine
    profiles:
      - extra
`

	t.Run("LoadOptions ExplicitServices rescues cross-profile service and transitive depends_on", func(t *testing.T) {
		dir := t.TempDir()
		composePath := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}

		cfg, err := Load(composePath, LoadOptions{
			Profiles:         []string{"metrics"},
			ExplicitServices: []string{"debug"},
		})
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if !reflect.DeepEqual(cfg.ActiveProfiles, []string{"metrics"}) {
			t.Fatalf("got ActiveProfiles %v, want [metrics]", cfg.ActiveProfiles)
		}
		if len(cfg.AllServices) != 6 {
			t.Fatalf("expected AllServices to retain all 6 services, got %d", len(cfg.AllServices))
		}
		for _, wantSvc := range []string{"web", "prom", "debug", "debug-db", "debug-init"} {
			if _, ok := cfg.Services[wantSvc]; !ok {
				t.Fatalf("expected service %q to be included, got %v", wantSvc, cfg.Services)
			}
		}
		if _, ok := cfg.Services["unused"]; ok {
			t.Fatalf("expected unrelated profiled service 'unused' to remain excluded")
		}
	})

	t.Run("Repeated FilterByProfiles uses AllServices to rescue previously excluded services", func(t *testing.T) {
		dir := t.TempDir()
		composePath := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}

		cfg, err := Load(composePath, LoadOptions{Profiles: []string{"metrics"}})
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if _, ok := cfg.Services["debug"]; ok {
			t.Fatalf("expected debug to be excluded on initial metrics filter")
		}

		refiltered := cfg.FilterByProfiles(cfg.ActiveProfiles, []string{"debug"})
		for _, wantSvc := range []string{"web", "prom", "debug", "debug-db", "debug-init"} {
			if _, ok := refiltered.Services[wantSvc]; !ok {
				t.Fatalf("expected service %q to be rescued from AllServices, got %v", wantSvc, refiltered.Services)
			}
		}
		if len(refiltered.AllServices) != 6 {
			t.Fatalf("expected AllServices to stay intact with 6 services, got %d", len(refiltered.AllServices))
		}
	})

	t.Run("COMPOSE_PROFILES from dotenv and env-file with precedence", func(t *testing.T) {
		dir := t.TempDir()
		composePath := filepath.Join(dir, "compose.yaml")
		dotenvPath := filepath.Join(dir, ".env")
		altEnvPath := filepath.Join(dir, "custom.env")

		if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}
		if err := os.WriteFile(dotenvPath, []byte("COMPOSE_PROFILES=debug\n"), 0644); err != nil {
			t.Fatalf("write .env failed: %v", err)
		}
		if err := os.WriteFile(altEnvPath, []byte("COMPOSE_PROFILES=metrics\n"), 0644); err != nil {
			t.Fatalf("write custom.env failed: %v", err)
		}

		cfgDotenv, err := Load(composePath)
		if err != nil {
			t.Fatalf("Load with .env failed: %v", err)
		}
		if !reflect.DeepEqual(cfgDotenv.ActiveProfiles, []string{"debug"}) {
			t.Fatalf("got ActiveProfiles %v, want [debug]", cfgDotenv.ActiveProfiles)
		}
		if _, ok := cfgDotenv.Services["debug"]; !ok {
			t.Fatalf("expected debug enabled from .env COMPOSE_PROFILES")
		}
		if _, ok := cfgDotenv.Services["debug-db"]; !ok {
			t.Fatalf("expected transitive dependency debug-db enabled")
		}

		cfgAltEnv, err := Load(composePath, LoadOptions{EnvFile: altEnvPath})
		if err != nil {
			t.Fatalf("Load with custom.env failed: %v", err)
		}
		if !reflect.DeepEqual(cfgAltEnv.ActiveProfiles, []string{"metrics"}) {
			t.Fatalf("got ActiveProfiles %v, want [metrics]", cfgAltEnv.ActiveProfiles)
		}
		if _, ok := cfgAltEnv.Services["prom"]; !ok {
			t.Fatalf("expected prom enabled from custom.env COMPOSE_PROFILES")
		}
		if _, ok := cfgAltEnv.Services["debug"]; ok {
			t.Fatalf("expected debug excluded when custom.env sets metrics")
		}

		t.Setenv("COMPOSE_PROFILES", "extra")
		cfgOSEnv, err := Load(composePath, LoadOptions{EnvFile: altEnvPath})
		if err != nil {
			t.Fatalf("Load with OS env failed: %v", err)
		}
		if !reflect.DeepEqual(cfgOSEnv.ActiveProfiles, []string{"extra"}) {
			t.Fatalf("OS env must override env-file, got %v", cfgOSEnv.ActiveProfiles)
		}
		if _, ok := cfgOSEnv.Services["unused"]; !ok {
			t.Fatalf("expected unused enabled by OS env extra profile")
		}

		cfgCLI, err := Load(composePath, LoadOptions{
			EnvFile:  altEnvPath,
			Profiles: []string{"metrics"},
		})
		if err != nil {
			t.Fatalf("Load with CLI profile failed: %v", err)
		}
		if !reflect.DeepEqual(cfgCLI.ActiveProfiles, []string{"metrics"}) {
			t.Fatalf("CLI profile must override OS env and env-file, got %v", cfgCLI.ActiveProfiles)
		}
		if _, ok := cfgCLI.Services["prom"]; !ok {
			t.Fatalf("expected prom enabled by CLI profile")
		}
		if _, ok := cfgCLI.Services["unused"]; ok {
			t.Fatalf("expected unused excluded when CLI profile overrides OS env")
		}
	})
}
