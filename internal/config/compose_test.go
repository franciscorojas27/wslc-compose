package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInterpolateExpressions(t *testing.T) {
	env := map[string]string{
		"SET":   "hello",
		"EMPTY": "",
	}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "${SET}", "hello"},
		{"bare", "$SET-world", "hello-world"},
		{"escaped", "$$SET $${SET}", "$SET ${SET}"},
		{"default_unset_colon", "${UNSET:-fallback}", "fallback"},
		{"default_empty_colon", "${EMPTY:-fallback}", "fallback"},
		{"default_empty_nocolon", "${EMPTY-fallback}", ""},
		{"default_unset_nocolon", "${UNSET-fallback}", "fallback"},
		{"alt_set_colon", "${SET:+yes}", "yes"},
		{"alt_empty_colon", "${EMPTY:+yes}", ""},
		{"alt_empty_nocolon", "${EMPTY+yes}", "yes"},
		{"alt_unset_nocolon", "${UNSET+yes}", ""},
		{"nested_default", "${UNSET:-${EMPTY:-deep}}", "deep"},
		{"required_set", "${SET:?must be set}", "hello"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Interpolate(tc.input, lookup)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := Interpolate("${EMPTY:?missing value}", lookup); err == nil || !strings.Contains(err.Error(), "missing value") {
		t.Fatalf("expected error for empty required var, got: %v", err)
	}
	if _, err := Interpolate("${UNSET?not defined}", lookup); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("expected error for unset required var, got: %v", err)
	}
}

func TestLoadComposeSpecAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("DB_PASS=from_dotenv\nOVERRIDE_ME=dotenv_val\nPASS_THROUGH=from_dotenv_pt\n"), 0644); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}

	t.Setenv("OVERRIDE_ME", "os_val")

	composeYAML := `
name: My App!
services:
  db:
    image: postgres:16-alpine
    container_name: custom_db
    environment:
      - POSTGRES_PASSWORD=${DB_PASS}
      - OVERRIDE_ME
      - PASS_THROUGH
    volumes:
      - db_data:/var/lib/postgresql/data:ro
      - type: bind
        source: ./init.sql
        target: /docker-entrypoint-initdb.d/init.sql
        read_only: true
    healthcheck:
      test: ["CMD", "pg_isready"]
      interval: 5s
      timeout: 3s
      retries: 5
    networks:
      backend:
        aliases:
          - database
        ipv4_address: 172.20.0.10
  api:
    build: .
    command: "go run ./cmd/server --port 8080"
    entrypoint: ["/bin/sh", "-c"]
    working_dir: /app
    user: "1000:1000"
    hostname: api-host
    labels:
      - tier=api
    extra_hosts:
      - "host.docker.internal:host-gateway"
    dns: 8.8.8.8
    privileged: true
    init: true
    shm_size: 128m
    tty: true
    stdin_open: true
    stop_grace_period: 15s
    env_file:
      - .env
    environment:
      APP_ENV: ${APP_ENV:-production}
      LITERAL: $$100
    depends_on:
      db:
        condition: service_healthy
    networks:
      - backend
  debug:
    image: alpine:latest
    profiles:
      - debug
networks:
  backend:
    driver: bridge
    internal: true
    ipam:
      config:
        - subnet: 172.20.0.0/16
          gateway: 172.20.0.1
  shared:
    external: true
volumes:
  db_data:
  custom_vol:
    name: my_exact_vol
`
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("failed to write compose.yaml: %v", err)
	}

	cfg, err := Load(composePath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.ProjectName() != "myapp" {
		t.Fatalf("unexpected project name: %q", cfg.ProjectName())
	}
	if cfg.ContainerName("db") != "custom_db" {
		t.Fatalf("expected custom_db, got %q", cfg.ContainerName("db"))
	}
	if cfg.ContainerName("api") != "myapp-api-1" {
		t.Fatalf("expected myapp-api-1, got %q", cfg.ContainerName("api"))
	}
	if cfg.NetworkName("backend") != "myapp_backend" {
		t.Fatalf("expected myapp_backend, got %q", cfg.NetworkName("backend"))
	}
	if cfg.NetworkName("shared") != "shared" {
		t.Fatalf("expected shared, got %q", cfg.NetworkName("shared"))
	}
	if cfg.VolumeName("db_data") != "myapp_db_data" {
		t.Fatalf("expected myapp_db_data, got %q", cfg.VolumeName("db_data"))
	}
	if cfg.VolumeName("custom_vol") != "my_exact_vol" {
		t.Fatalf("expected my_exact_vol, got %q", cfg.VolumeName("custom_vol"))
	}

	db := cfg.Services["db"]
	if db.Environment["POSTGRES_PASSWORD"] != "from_dotenv" {
		t.Fatalf("unexpected db password: %q", db.Environment["POSTGRES_PASSWORD"])
	}
	if db.Environment["OVERRIDE_ME"] != "os_val" {
		t.Fatalf("OS env must override .env, got %q", db.Environment["OVERRIDE_ME"])
	}
	if db.Environment["PASS_THROUGH"] != "from_dotenv_pt" {
		t.Fatalf("expected .env pass-through, got %q", db.Environment["PASS_THROUGH"])
	}
	if len(db.Volumes) != 2 || !db.Volumes[0].ReadOnly || !db.Volumes[1].ReadOnly || db.Volumes[1].Type != "bind" {
		t.Fatalf("unexpected volumes: %+v", db.Volumes)
	}

	api := cfg.Services["api"]
	if api.Build.Context != "." {
		t.Fatalf("expected build context '.', got %q", api.Build.Context)
	}
	if !reflect.DeepEqual([]string(api.Command), []string{"go", "run", "./cmd/server", "--port", "8080"}) {
		t.Fatalf("unexpected command: %#v", api.Command)
	}
	if api.Environment["APP_ENV"] != "production" || api.Environment["LITERAL"] != "$100" {
		t.Fatalf("unexpected api env: %#v", api.Environment)
	}
	if api.DependsOn["db"].Condition != "service_healthy" {
		t.Fatalf("unexpected depends_on condition: %+v", api.DependsOn)
	}

	filtered := cfg.FilterByProfiles(nil, nil)
	if _, ok := filtered.Services["debug"]; ok {
		t.Fatalf("expected debug service to be excluded without profile")
	}

	withProfile := cfg.FilterByProfiles([]string{"debug"}, nil)
	if _, ok := withProfile.Services["debug"]; !ok {
		t.Fatalf("expected debug service to be included with profile")
	}

	withExplicit := cfg.FilterByProfiles(nil, []string{"debug"})
	if _, ok := withExplicit.Services["debug"]; !ok {
		t.Fatalf("expected debug service to be included when explicitly requested")
	}
}
