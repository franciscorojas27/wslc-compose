package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestYAMLMergeKeysInCustomUnmarshallers(t *testing.T) {
	dir := t.TempDir()
	composeYAML := `
x-args-a: &args_a
  A: from_a
  B: from_a
  SHARED: from_a
x-args-b: &args_b
  B: from_b
  C: from_b
  SHARED: from_b

x-hosts-a: &hosts_a
  alpha.local: 10.0.0.1
  beta.local: 10.0.0.2
x-hosts-b: &hosts_b
  beta.local: 10.0.0.99
  gamma.local: 10.0.0.3

x-env-root: &env_root
  ROOT_KEY: root_val
  MID_KEY: root_mid
x-env-a: &env_a
  <<: *env_root
  MID_KEY: a_mid
  PRIO_KEY: from_a
x-env-b: &env_b
  PRIO_KEY: from_b
  TAIL_KEY: from_b

x-net-a: &net_a
  front:
    aliases:
      - front-a
  shared_net:
    ipv4_address: 172.28.0.10
x-net-b: &net_b
  shared_net:
    ipv4_address: 172.28.0.99
  back:
    aliases:
      - back-b

x-dep-a: &dep_a
  db:
    condition: service_healthy
  cache:
    condition: service_started
x-dep-b: &dep_b
  cache:
    condition: service_completed_successfully
  init_job:
    condition: service_completed_successfully

services:
  db:
    image: postgres:16-alpine
  cache:
    image: redis:7-alpine
  init_job:
    image: alpine:latest
  app:
    image: myapp:latest
    build:
      context: .
      args:
        <<: [*args_a, *args_b]
        SHARED: explicit_override
    extra_hosts:
      <<: [*hosts_a, *hosts_b]
      gamma.local: 10.0.0.30
    environment:
      <<: [*env_a, *env_b]
      TAIL_KEY: explicit_tail
    networks:
      <<: [*net_a, *net_b]
      back:
        aliases:
          - explicit-back
    depends_on:
      <<: [*dep_a, *dep_b]
      init_job:
        condition: service_started
networks:
  front: {}
  back: {}
  shared_net: {}
`
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	cfg, err := Load(composePath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	app := cfg.Services["app"]

	wantArgs := map[string]string{
		"A":      "from_a",
		"B":      "from_a",
		"C":      "from_b",
		"SHARED": "explicit_override",
	}
	if !reflect.DeepEqual(app.Build.Args, wantArgs) {
		t.Fatalf("BuildArgs got %#v, want %#v", app.Build.Args, wantArgs)
	}

	wantHosts := ExtraHosts{
		"alpha.local:10.0.0.1",
		"beta.local:10.0.0.2",
		"gamma.local:10.0.0.30",
	}
	if !reflect.DeepEqual(app.ExtraHosts, wantHosts) {
		t.Fatalf("ExtraHosts got %#v, want %#v", app.ExtraHosts, wantHosts)
	}

	wantEnv := EnvironmentMap{
		"ROOT_KEY": "root_val",
		"MID_KEY":  "a_mid",
		"PRIO_KEY": "from_a",
		"TAIL_KEY": "explicit_tail",
	}
	if !reflect.DeepEqual(app.Environment, wantEnv) {
		t.Fatalf("Environment got %#v, want %#v", app.Environment, wantEnv)
	}

	wantNets := ServiceNetworks{
		"front":      {Aliases: []string{"front-a"}},
		"shared_net": {IPv4Address: "172.28.0.10"},
		"back":       {Aliases: []string{"explicit-back"}},
	}
	if !reflect.DeepEqual(app.Networks, wantNets) {
		t.Fatalf("Networks got %#v, want %#v", app.Networks, wantNets)
	}

	wantDeps := DependsOnConfig{
		"db":       {Condition: "service_healthy"},
		"cache":    {Condition: "service_started"},
		"init_job": {Condition: "service_started"},
	}
	if !reflect.DeepEqual(app.DependsOn, wantDeps) {
		t.Fatalf("DependsOn got %#v, want %#v", app.DependsOn, wantDeps)
	}
}

func TestSingleLetterNamedVolumesAndWindowsDrives(t *testing.T) {
	cases := []struct {
		spec       string
		wantType   string
		wantSource string
		wantTarget string
		wantRO     bool
	}{
		{"v:/data", "volume", "v", "/data", false},
		{"v:/data:ro", "volume", "v", "/data", true},
		{"a:/var/lib/app:rw", "volume", "a", "/var/lib/app", false},
		{"C:/host:/target", "bind", "C:/host", "/target", false},
		{"C:/host:/target:ro", "bind", "C:/host", "/target", true},
		{`C:\host:/target`, "bind", `C:\host`, "/target", false},
		{`C:\host:/target:ro`, "bind", `C:\host`, "/target", true},
		{`D:\dir\sub:/mnt/sub:ro`, "bind", `D:\dir\sub`, "/mnt/sub", true},
	}

	for _, tc := range cases {
		got := parseShortVolume(tc.spec)
		if got.Type != tc.wantType || got.Source != tc.wantSource || got.Target != tc.wantTarget || got.ReadOnly != tc.wantRO {
			t.Fatalf("parseShortVolume(%q) = %+v, want type=%q src=%q dst=%q ro=%v",
				tc.spec, got, tc.wantType, tc.wantSource, tc.wantTarget, tc.wantRO)
		}
	}
}

func TestLongSyntaxMountWithoutTypeInferenceAndPathResolution(t *testing.T) {
	dir := t.TempDir()
	composeYAML := `
services:
  app:
    image: alpine:latest
    volumes:
      - source: ./rel-data
        target: /app/data
      - source: named_vol
        target: /app/named
      - target: /app/anon
volumes:
  named_vol: {}
`
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	cfg, err := Load(composePath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	vols := cfg.Services["app"].Volumes
	if len(vols) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(vols))
	}
	wantRel := filepath.Join(dir, "rel-data")
	if vols[0].Type != "bind" || vols[0].Source != wantRel {
		t.Fatalf("mount[0] got %+v, want type=bind source=%q", vols[0], wantRel)
	}
	if vols[1].Type != "volume" || vols[1].Source != "named_vol" {
		t.Fatalf("mount[1] got %+v, want type=volume source=named_vol", vols[1])
	}
	if vols[2].Type != "volume" || vols[2].Source != "" || vols[2].Target != "/app/anon" {
		t.Fatalf("mount[2] got %+v, want anonymous volume", vols[2])
	}
}

func TestParseDurationSecondsCeilSubSecond(t *testing.T) {
	cases := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{"0s", 0, true},
		{"0", 0, true},
		{"500ms", 1, true},
		{"1ms", 1, true},
		{"1s", 1, true},
		{"1500ms", 2, true},
		{"10s", 10, true},
		{"-5s", 0, false},
		{"", 0, false},
	}

	for _, tc := range cases {
		got, ok := ParseDurationSeconds(tc.raw)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("ParseDurationSeconds(%q) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}
