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

func TestVolumeDriverOptsLabelsAndInterpolation(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	envContent := "VOL_DRIVER=local\nVOL_NAME=custom_pg_data\nNFS_DEVICE=:/srv/data\nENV_TIER=staging\nLABEL_OWNER=platform\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}

	composeYAML := `
name: voltest
services:
  app:
    image: alpine:latest
volumes:
  nfs_data:
    driver: ${VOL_DRIVER}
    name: ${VOL_NAME}
    driver_opts:
      type: nfs
      o: addr=10.0.0.1,rw
      device: ${NFS_DEVICE}
    labels:
      com.example.tier: ${ENV_TIER}
      com.example.owner: ${LABEL_OWNER}
  list_labels_vol:
    driver: local
    labels:
      - app=backend
      - env=${ENV_TIER}
      - bare_flag
  default_vol:
  vhd_vol:
    driver: vhd
  ext_vol:
    external: true
  ext_named:
    external: true
    name: existing_shared_vol
`
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("failed to write compose.yaml: %v", err)
	}

	cfg, err := Load(composePath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	nfsVol, ok := cfg.Volumes["nfs_data"]
	if !ok {
		t.Fatalf("expected nfs_data volume to be loaded")
	}
	if nfsVol.Driver != "local" {
		t.Fatalf("expected interpolated driver 'local', got %q", nfsVol.Driver)
	}
	if nfsVol.Name != "custom_pg_data" || cfg.VolumeName("nfs_data") != "custom_pg_data" {
		t.Fatalf("expected interpolated volume name 'custom_pg_data', got field=%q resolved=%q", nfsVol.Name, cfg.VolumeName("nfs_data"))
	}
	wantDriverOpts := map[string]string{
		"type":   "nfs",
		"o":      "addr=10.0.0.1,rw",
		"device": ":/srv/data",
	}
	if !reflect.DeepEqual(nfsVol.DriverOpts, wantDriverOpts) {
		t.Fatalf("got driver_opts %#v, want %#v", nfsVol.DriverOpts, wantDriverOpts)
	}
	wantMapLabels := LabelMap{
		"com.example.tier":  "staging",
		"com.example.owner": "platform",
	}
	if !reflect.DeepEqual(nfsVol.Labels, wantMapLabels) {
		t.Fatalf("got map labels %#v, want %#v", nfsVol.Labels, wantMapLabels)
	}

	listVol, ok := cfg.Volumes["list_labels_vol"]
	if !ok {
		t.Fatalf("expected list_labels_vol volume to be loaded")
	}
	if cfg.VolumeName("list_labels_vol") != "voltest_list_labels_vol" {
		t.Fatalf("expected project-prefixed volume name 'voltest_list_labels_vol', got %q", cfg.VolumeName("list_labels_vol"))
	}
	wantListLabels := LabelMap{
		"app":       "backend",
		"env":       "staging",
		"bare_flag": "",
	}
	if !reflect.DeepEqual(listVol.Labels, wantListLabels) {
		t.Fatalf("got list labels %#v, want %#v", listVol.Labels, wantListLabels)
	}

	if cfg.VolumeName("default_vol") != "voltest_default_vol" {
		t.Fatalf("expected project-prefixed default_vol name 'voltest_default_vol', got %q", cfg.VolumeName("default_vol"))
	}
	if cfg.Volumes["default_vol"].Driver != "" {
		t.Fatalf("expected omitted volume driver to remain empty, got %q", cfg.Volumes["default_vol"].Driver)
	}
	if cfg.Volumes["vhd_vol"].Driver != "vhd" {
		t.Fatalf("expected vhd_vol driver 'vhd', got %q", cfg.Volumes["vhd_vol"].Driver)
	}
	if !cfg.Volumes["ext_vol"].External || cfg.VolumeName("ext_vol") != "ext_vol" {
		t.Fatalf("expected external volume ext_vol to preserve raw name, got external=%v name=%q", cfg.Volumes["ext_vol"].External, cfg.VolumeName("ext_vol"))
	}
	if !cfg.Volumes["ext_named"].External || cfg.VolumeName("ext_named") != "existing_shared_vol" {
		t.Fatalf("expected external named volume to resolve to existing_shared_vol, got external=%v name=%q", cfg.Volumes["ext_named"].External, cfg.VolumeName("ext_named"))
	}
}

func TestBaseDirEnvFileAndBindMountResolution(t *testing.T) {
	root := t.TempDir()
	subDir := filepath.Join(root, "other-folder")
	if err := os.MkdirAll(filepath.Join(subDir, "nginx"), 0755); err != nil {
		t.Fatalf("failed to create nested dirs: %v", err)
	}

	composeYAML := `
name: nestedapp
services:
  single:
    image: alpine:latest
    env_file: .env.single
    volumes:
      - ./nginx/default.conf:/etc/nginx/conf.d/default.conf:ro
      - db_data:/var/lib/data
  multi:
    image: alpine:latest
    env_file:
      - .env.first
      - sub/.env.second
    volumes:
      - type: bind
        source: ./data
        target: /data
volumes:
  db_data:
`
	composePath := filepath.Join(subDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("failed to write compose.yaml: %v", err)
	}

	cfg, err := Load(composePath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.BaseDir != subDir {
		t.Fatalf("expected BaseDir %q, got %q", subDir, cfg.BaseDir)
	}

	single := cfg.Services["single"]
	wantSingleEnv := StringList{filepath.Join(subDir, ".env.single")}
	if !reflect.DeepEqual(single.EnvFile, wantSingleEnv) {
		t.Fatalf("expected single env_file %v, got %v", wantSingleEnv, single.EnvFile)
	}
	wantConfSource := filepath.Join(subDir, "nginx", "default.conf")
	if len(single.Volumes) != 2 || single.Volumes[0].Source != wantConfSource || single.Volumes[0].Type != "bind" {
		t.Fatalf("expected short bind source %q, got %+v", wantConfSource, single.Volumes)
	}
	if single.Volumes[1].Source != "db_data" || single.Volumes[1].Type != "volume" {
		t.Fatalf("expected named volume source 'db_data' unchanged, got %+v", single.Volumes[1])
	}

	multi := cfg.Services["multi"]
	wantMultiEnv := StringList{
		filepath.Join(subDir, ".env.first"),
		filepath.Join(subDir, "sub", ".env.second"),
	}
	if !reflect.DeepEqual(multi.EnvFile, wantMultiEnv) {
		t.Fatalf("expected multi env_file %v, got %v", wantMultiEnv, multi.EnvFile)
	}
	wantDataSource := filepath.Join(subDir, "data")
	if len(multi.Volumes) != 1 || multi.Volumes[0].Source != wantDataSource || multi.Volumes[0].Type != "bind" {
		t.Fatalf("expected long bind source %q, got %+v", wantDataSource, multi.Volumes)
	}
}

func TestASTInterpolationTypedScalarsAndAnchors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SVC_PRIV", "true")
	t.Setenv("HC_RETRIES", "7")
	t.Setenv("ZERO_CODE", "007")

	composeYAML := `
x-defaults: &defaults
  environment:
    SHARED_LITERAL: $$100
    CODE: ${ZERO_CODE}
services:
  app:
    <<: *defaults
    image: alpine:latest
    privileged: ${SVC_PRIV}
    healthcheck:
      test: ["CMD", "true"]
      retries: ${HC_RETRIES}
`
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	app := cfg.Services["app"]
	if !app.Privileged {
		t.Fatalf("expected privileged=true from interpolated bool")
	}
	if app.HealthCheck == nil || app.HealthCheck.Retries != 7 {
		t.Fatalf("expected retries=7, got %+v", app.HealthCheck)
	}
	if app.Environment["SHARED_LITERAL"] != "$100" {
		t.Fatalf("expected anchor escaped literal '$100', got %q", app.Environment["SHARED_LITERAL"])
	}
	if app.Environment["CODE"] != "007" {
		t.Fatalf("expected leading-zero string '007', got %q", app.Environment["CODE"])
	}
}

func TestComposeSpecBuildArgsPortsExtraHostsAndResolvePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARG_FROM_HOST", "secret_val")

	composeYAML := `
services:
  web:
    image: nginx:alpine
    build:
      context: ./app
      args:
        - EXPLICIT=yes
        - ARG_FROM_HOST
        - UNSET_ARG
    ports:
      - 3000
      - "8080:80"
      - target: 443
        published: 8443
        host_ip: "::1"
        protocol: tcp
        mode: ingress
      - target: 9000
    extra_hosts:
      single.local: 10.0.0.1
      multi.local:
        - 10.0.0.2
        - 10.0.0.3
`
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	web := cfg.Services["web"]
	wantArgs := map[string]string{
		"EXPLICIT":      "yes",
		"ARG_FROM_HOST": "secret_val",
	}
	if !reflect.DeepEqual(web.Build.Args, wantArgs) {
		t.Fatalf("got build.args %#v, want %#v", web.Build.Args, wantArgs)
	}
	wantPorts := PortList{"3000", "8080:80", "[::1]:8443:443/tcp", "9000"}
	if !reflect.DeepEqual(web.Ports, wantPorts) {
		t.Fatalf("got ports %#v, want %#v", web.Ports, wantPorts)
	}
	wantHosts := ExtraHosts{"single.local:10.0.0.1", "multi.local:10.0.0.2", "multi.local:10.0.0.3"}
	if !reflect.DeepEqual(web.ExtraHosts, wantHosts) {
		t.Fatalf("got extra_hosts %#v, want %#v", web.ExtraHosts, wantHosts)
	}
	if got := cfg.ResolvePath("/var/run/docker.sock"); got != "/var/run/docker.sock" {
		t.Fatalf("expected POSIX path preserved, got %q", got)
	}
	if got := cfg.ResolvePath("/mnt/c/foo/../bar"); got != "/mnt/c/bar" {
		t.Fatalf("expected cleaned POSIX path, got %q", got)
	}
}
