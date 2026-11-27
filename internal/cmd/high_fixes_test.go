package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHighFix1CmdLoadComposeProfilesAndExplicitServices(t *testing.T) {
	origFiles := append([]string(nil), composeFiles...)
	origProject := projectName
	origEnvFile := envFile
	origProfiles := append([]string(nil), profiles...)

	t.Cleanup(func() {
		composeFiles = origFiles
		projectName = origProject
		envFile = origEnvFile
		profiles = origProfiles
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rootCmd.PersistentFlags(), "project-name")
		resetFlag(rootCmd.PersistentFlags(), "env-file")
		resetFlag(rootCmd.PersistentFlags(), "profile")
	})

	composeYAML := `
name: cmdprof
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
      debug-helper:
        condition: service_started
  debug-helper:
    image: busybox:latest
    profiles:
      - helper
  other:
    image: redis:alpine
    profiles:
      - other
`

	t.Run("CLI --profile metrics with explicit service debug in another profile", func(t *testing.T) {
		dir := t.TempDir()
		cPath := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(cPath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}

		composeFiles = []string{cPath}
		projectName = ""
		envFile = ""
		profiles = []string{"metrics"}

		cfg, err := loadCompose([]string{"debug"})
		if err != nil {
			t.Fatalf("loadCompose failed: %v", err)
		}

		for _, want := range []string{"web", "prom", "debug", "debug-helper"} {
			if _, ok := cfg.Services[want]; !ok {
				t.Fatalf("expected service %q in loaded services, got %v", want, cfg.Services)
			}
		}
		if _, ok := cfg.Services["other"]; ok {
			t.Fatalf("expected 'other' profile service to remain excluded")
		}
	})

	t.Run("COMPOSE_PROFILES defined only in .env is preserved by loadCompose", func(t *testing.T) {
		dir := t.TempDir()
		cPath := filepath.Join(dir, "compose.yaml")
		dotenvPath := filepath.Join(dir, ".env")
		if err := os.WriteFile(cPath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}
		if err := os.WriteFile(dotenvPath, []byte("COMPOSE_PROFILES=debug\n"), 0644); err != nil {
			t.Fatalf("write .env failed: %v", err)
		}

		composeFiles = []string{cPath}
		projectName = ""
		envFile = ""
		profiles = nil

		cfg, err := loadCompose(nil)
		if err != nil {
			t.Fatalf("loadCompose failed: %v", err)
		}

		if !reflect.DeepEqual(cfg.ActiveProfiles, []string{"debug"}) {
			t.Fatalf("got ActiveProfiles %v, want [debug]", cfg.ActiveProfiles)
		}
		for _, want := range []string{"web", "debug", "debug-helper"} {
			if _, ok := cfg.Services[want]; !ok {
				t.Fatalf("expected service %q enabled via .env COMPOSE_PROFILES, got %v", want, cfg.Services)
			}
		}
		if _, ok := cfg.Services["prom"]; ok {
			t.Fatalf("expected metrics service 'prom' excluded")
		}
	})

	t.Run("COMPOSE_PROFILES defined only in --env-file is preserved by loadCompose and loadComposeAll", func(t *testing.T) {
		dir := t.TempDir()
		cPath := filepath.Join(dir, "compose.yaml")
		altEnv := filepath.Join(dir, "staging.env")
		if err := os.WriteFile(cPath, []byte(composeYAML), 0644); err != nil {
			t.Fatalf("write compose failed: %v", err)
		}
		if err := os.WriteFile(altEnv, []byte("COMPOSE_PROFILES=metrics\n"), 0644); err != nil {
			t.Fatalf("write staging.env failed: %v", err)
		}

		composeFiles = []string{cPath}
		projectName = ""
		envFile = altEnv
		profiles = nil

		cfg, err := loadCompose(nil)
		if err != nil {
			t.Fatalf("loadCompose failed: %v", err)
		}
		if _, ok := cfg.Services["prom"]; !ok {
			t.Fatalf("expected prom enabled via --env-file COMPOSE_PROFILES")
		}
		if _, ok := cfg.Services["debug"]; ok {
			t.Fatalf("expected debug excluded")
		}

		cfgAll, err := loadComposeAll()
		if err != nil {
			t.Fatalf("loadComposeAll failed: %v", err)
		}
		if _, ok := cfgAll.Services["prom"]; !ok {
			t.Fatalf("expected prom enabled in loadComposeAll via --env-file")
		}
		if _, ok := cfgAll.Services["debug"]; ok {
			t.Fatalf("expected debug excluded in loadComposeAll when --env-file activates metrics")
		}
	})
}
