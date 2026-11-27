package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestAuditInterpolateEdgeCases(t *testing.T) {
	env := map[string]string{
		"FOO":   "bar",
		"EMPTY": "",
	}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}

	t.Run("trailing dollar sign preserved", func(t *testing.T) {
		got, err := Interpolate("cost$", lookup)
		if err != nil || got != "cost$" {
			t.Fatalf("got %q err=%v, want %q", got, err, "cost$")
		}
	})

	t.Run("escaped dollar before braced variable", func(t *testing.T) {
		got, err := Interpolate("$$${FOO}", lookup)
		if err != nil || got != "$bar" {
			t.Fatalf("got %q err=%v, want %q", got, err, "$bar")
		}
	})

	t.Run("unclosed brace returns error", func(t *testing.T) {
		if _, err := Interpolate("${FOO", lookup); err == nil {
			t.Fatalf("expected error for unclosed brace")
		}
	})

	t.Run("nested required error not evaluated when outer var is set", func(t *testing.T) {
		got, err := Interpolate("${FOO:-${UNSET:?should not trigger}}", lookup)
		if err != nil || got != "bar" {
			t.Fatalf("got %q err=%v, want %q", got, err, "bar")
		}
	})
}

func TestAuditWindowsShortVolumeParsing(t *testing.T) {
	cases := []struct {
		spec       string
		wantType   string
		wantSource string
		wantTarget string
		wantRO     bool
	}{
		{`C:\data:/app`, "bind", `C:\data`, "/app", false},
		{`C:\data:/app:ro`, "bind", `C:\data`, "/app", true},
		{`C:/data/sub:/app/sub:ro`, "bind", `C:/data/sub`, "/app/sub", true},
		{`\\server\share\dir:/mnt/share`, "bind", `\\server\share\dir`, "/mnt/share", false},
		{`db_data:/var/lib/data`, "volume", "db_data", "/var/lib/data", false},
	}

	for _, tc := range cases {
		got := parseShortVolume(tc.spec)
		if got.Type != tc.wantType || got.Source != tc.wantSource || got.Target != tc.wantTarget || got.ReadOnly != tc.wantRO {
			t.Fatalf("parseShortVolume(%q) = %+v", tc.spec, got)
		}
	}
}

func TestAuditBugRawInterpolationEvaluatesYAMLComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	content := "services:\n  app:\n    # ${MISSING_REQUIRED:?must be set}\n    image: alpine:latest\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("YAML comment caused Load to fail: %v", err)
	}
}

func TestAuditBugRawInterpolationSpecialCharsAndYAMLInjection(t *testing.T) {
	t.Run("colon and space", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AUDIT_COLON_VAL", "user: secret")
		path := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(path, []byte("services:\n  app:\n    image: alpine:latest\n    environment:\n      APP_VAL: ${AUDIT_COLON_VAL}\n"), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed on ': ': %v", err)
		}
		if cfg.Services["app"].Environment["APP_VAL"] != "user: secret" {
			t.Fatalf("got %q", cfg.Services["app"].Environment["APP_VAL"])
		}
	})

	t.Run("hash truncation", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AUDIT_HASH_VAL", "pass #notacomment")
		path := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(path, []byte("services:\n  app:\n    image: alpine:latest\n    environment:\n      APP_PASS: ${AUDIT_HASH_VAL}\n"), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if got := cfg.Services["app"].Environment["APP_PASS"]; got != "pass #notacomment" {
			t.Fatalf("got %q, want %q", got, "pass #notacomment")
		}
	})

	t.Run("yaml injection", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AUDIT_INJECT_VAL", "ok\n    privileged: true")
		path := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(path, []byte("services:\n  app:\n    image: alpine:latest\n    hostname: ${AUDIT_INJECT_VAL}\n"), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Services["app"].Privileged {
			t.Fatalf("YAML injection set privileged=true")
		}
	})
}

func TestAuditBugEmptyInterpolationLeaksHostEnvInEnvironmentMap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TARGET_SECRET", "leaked_host_secret")
	t.Setenv("PASSTHROUGH_VAR", "host_passthrough")
	path := filepath.Join(dir, "compose.yaml")
	content := "services:\n  app:\n    image: alpine:latest\n    environment:\n      TARGET_SECRET: ${UNSET_SOURCE_VAR}\n      QUOTED_UNSET: \"${UNSET_SOURCE_VAR}\"\n      EXPLICIT_EMPTY: \"\"\n      EMPTY_EXPECTED: ${UNSET_SOURCE_VAR}\n      PASSTHROUGH_VAR:\n      UNSET_PASSTHROUGH:\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	env := cfg.Services["app"].Environment
	for _, key := range []string{"TARGET_SECRET", "QUOTED_UNSET", "EXPLICIT_EMPTY", "EMPTY_EXPECTED"} {
		if val, ok := env[key]; !ok || val != "" {
			t.Fatalf("%s: got present=%v val=%q, want present=true val=\"\"", key, ok, val)
		}
	}
	if val, ok := env["PASSTHROUGH_VAR"]; !ok || val != "host_passthrough" {
		t.Fatalf("PASSTHROUGH_VAR: got present=%v val=%q, want host_passthrough", ok, val)
	}
	if _, ok := env["UNSET_PASSTHROUGH"]; ok {
		t.Fatalf("UNSET_PASSTHROUGH should be omitted when unset on host")
	}
}

func TestAuditBugResolvePathCorruptsPOSIXAbsolutePathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	content := "services:\n  app:\n    image: alpine:latest\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n      - /mnt/c/workspace/../data:/data\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.Services["app"].Volumes[0].Source; got != "/var/run/docker.sock" {
		t.Fatalf("got %q, want %q", got, "/var/run/docker.sock")
	}
	if got := cfg.Services["app"].Volumes[1].Source; got != "/mnt/c/data" {
		t.Fatalf("got %q, want %q", got, "/mnt/c/data")
	}
}

func TestAuditBugEnvFileQuotedValueWithInlineComment(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		`DOUBLE="hello\nworld" # inline comment`,
		`SINGLE='plain value' # inline comment`,
		"TAB_COMMENT=value\t# tab comment",
		`HASH_DOUBLE="keep # hash" # trailing comment`,
		`HASH_SINGLE='keep # hash' # trailing comment`,
		`ESCAPED_QUOTE="say \"hi\" # keep" # trailing comment`,
		`ESCAPED_SLASH="trail\\" # trailing comment`,
	}, "\n")
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	vals, err := LoadEnvFile(envPath, true)
	if err != nil {
		t.Fatalf("LoadEnvFile failed: %v", err)
	}
	want := map[string]string{
		"DOUBLE":        "hello\nworld",
		"SINGLE":        "plain value",
		"TAB_COMMENT":   "value",
		"HASH_DOUBLE":   "keep # hash",
		"HASH_SINGLE":   "keep # hash",
		"ESCAPED_QUOTE": `say "hi" # keep`,
		"ESCAPED_SLASH": `trail\`,
	}
	if !reflect.DeepEqual(vals, want) {
		t.Fatalf("unexpected parsed env:\ngot:  %#v\nwant: %#v", vals, want)
	}
}

func TestAuditBugComposeSpecYAMLSyntaxCompatibility(t *testing.T) {
	t.Run("build_args_list", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "compose.yaml")
		content := "services:\n  app:\n    build:\n      context: .\n      args:\n        - GO_VERSION=1.27\n        - CGO_ENABLED=0\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		want := map[string]string{"GO_VERSION": "1.27", "CGO_ENABLED": "0"}
		if !reflect.DeepEqual(cfg.Services["app"].Build.Args, want) {
			t.Fatalf("got build.args %#v, want %#v", cfg.Services["app"].Build.Args, want)
		}
	})

	t.Run("ports_long", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "compose.yaml")
		content := "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"80:80\"\n      - target: 80\n        published: 8080\n      - target: 53\n        published: 5353\n        host_ip: 127.0.0.1\n        protocol: udp\n        mode: host\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		want := PortList{"80:80", "8080:80", "127.0.0.1:5353:53/udp"}
		if !reflect.DeepEqual(cfg.Services["web"].Ports, want) {
			t.Fatalf("got ports %#v, want %#v", cfg.Services["web"].Ports, want)
		}
	})

	t.Run("extra_hosts_map", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "compose.yaml")
		content := "services:\n  web:\n    image: nginx:alpine\n    extra_hosts:\n      somehost: 162.242.195.82\n      otherhost: 50.31.209.229\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		want := ExtraHosts{"somehost:162.242.195.82", "otherhost:50.31.209.229"}
		if !reflect.DeepEqual(cfg.Services["web"].ExtraHosts, want) {
			t.Fatalf("got extra_hosts %#v, want %#v", cfg.Services["web"].ExtraHosts, want)
		}
	})
}

func TestAuditBugLoadOptionsProfilesIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yaml")
	content := "services:\n  web:\n    image: nginx:alpine\n  debug:\n    image: alpine:latest\n    profiles:\n      - debug\n  metrics:\n    image: prom/prometheus\n    profiles:\n      - metrics\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	cfg, err := Load(path, LoadOptions{Profiles: []string{"prod"}})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if _, exists := cfg.Services["web"]; !exists {
		t.Fatalf("web service without profiles must remain active")
	}
	if _, exists := cfg.Services["debug"]; exists {
		t.Fatalf("debug service was not filtered by LoadOptions.Profiles")
	}

	t.Setenv("COMPOSE_PROFILES", "debug, metrics")
	cfgEnv, err := Load(path)
	if err != nil {
		t.Fatalf("Load with COMPOSE_PROFILES failed: %v", err)
	}
	if _, ok := cfgEnv.Services["web"]; !ok {
		t.Fatalf("expected web to remain active")
	}
	if _, ok := cfgEnv.Services["debug"]; !ok {
		t.Fatalf("expected debug to be enabled by COMPOSE_PROFILES")
	}
	if _, ok := cfgEnv.Services["metrics"]; !ok {
		t.Fatalf("expected metrics to be enabled by COMPOSE_PROFILES")
	}
}

func TestAuditBugSplitShellWordsCorruptsWindowsBackslashesInDoubleQuotes(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{`echo "C:\Users\app\file.txt"`, []string{"echo", `C:\Users\app\file.txt`}},
		{`echo "say \"hi\" \\ \$HOME \` + "`cmd`" + `"`, []string{"echo", "say \"hi\" \\ $HOME `cmd`"}},
	}
	for _, tc := range cases {
		got := splitShellWords(tc.input)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("splitShellWords(%q) = %#v, want %#v", tc.input, got, tc.want)
		}
	}
}

func TestAuditMultiFileMerging(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "compose.yaml")
	overridePath := filepath.Join(dir, "compose.override.yaml")

	baseYAML := `
name: baseapp
services:
  web:
    image: nginx:1.25
    privileged: true
    build: ./web
    environment:
      - A=1
      - B=2
    ports:
      - "80:80"
    volumes:
      - ./html:/usr/share/nginx/html
    extra_hosts:
      - "api.local:10.0.0.1"
`
	overrideYAML := `
services:
  web:
    privileged: false
    build:
      dockerfile: Dockerfile.dev
      args:
        - MODE=dev
    environment:
      B: overridden
      C: "3"
    ports:
      - target: 443
        published: 8443
    volumes:
      - ./dev-html:/usr/share/nginx/html
      - ./logs:/var/log/nginx
    extra_hosts:
      api.local: 10.0.0.2
      db.local: 10.0.0.3
  worker:
    image: alpine:latest
`
	if err := os.WriteFile(basePath, []byte(baseYAML), 0644); err != nil {
		t.Fatalf("write base failed: %v", err)
	}
	if err := os.WriteFile(overridePath, []byte(overrideYAML), 0644); err != nil {
		t.Fatalf("write override failed: %v", err)
	}

	verifyMerged := func(t *testing.T, cfg ComposeFile) {
		t.Helper()
		if cfg.ProjectName() != "baseapp" {
			t.Fatalf("got project name %q, want baseapp", cfg.ProjectName())
		}
		web, ok := cfg.Services["web"]
		if !ok {
			t.Fatalf("missing web service")
		}
		if web.Image != "nginx:1.25" {
			t.Fatalf("got image %q, want nginx:1.25", web.Image)
		}
		if web.Privileged {
			t.Fatalf("expected privileged=false override to take effect")
		}
		if web.Build.Context != "./web" || web.Build.Dockerfile != "Dockerfile.dev" || web.Build.Args["MODE"] != "dev" {
			t.Fatalf("unexpected merged build: %+v", web.Build)
		}
		wantEnv := EnvironmentMap{"A": "1", "B": "overridden", "C": "3"}
		if !reflect.DeepEqual(web.Environment, wantEnv) {
			t.Fatalf("got env %#v, want %#v", web.Environment, wantEnv)
		}
		wantPorts := PortList{"80:80", "8443:443"}
		if !reflect.DeepEqual(web.Ports, wantPorts) {
			t.Fatalf("got ports %#v, want %#v", web.Ports, wantPorts)
		}
		if len(web.Volumes) != 2 || web.Volumes[0].Source != filepath.Join(dir, "dev-html") || web.Volumes[1].Source != filepath.Join(dir, "logs") {
			t.Fatalf("unexpected merged volumes: %+v", web.Volumes)
		}
		wantHosts := ExtraHosts{"api.local:10.0.0.2", "db.local:10.0.0.3"}
		if !reflect.DeepEqual(web.ExtraHosts, wantHosts) {
			t.Fatalf("got extra_hosts %#v, want %#v", web.ExtraHosts, wantHosts)
		}
		if _, ok := cfg.Services["worker"]; !ok {
			t.Fatalf("expected worker service from override file")
		}
	}

	t.Run("LoadOptions.Files", func(t *testing.T) {
		cfg, err := Load("", LoadOptions{Files: []string{basePath, overridePath}})
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		verifyMerged(t, cfg)
	})

	t.Run("COMPOSE_FILE", func(t *testing.T) {
		t.Setenv("COMPOSE_FILE", basePath+string(os.PathListSeparator)+overridePath)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load with COMPOSE_FILE failed: %v", err)
		}
		verifyMerged(t, cfg)
	})
}
