package runtime

import (
	"reflect"
	"strings"
	"testing"
	"wslc-compose/internal/config"
)

func TestReadStreamLinesHandlesCRAndLargeLines(t *testing.T) {
	largeLine := strings.Repeat("a", maxStreamLineBytes+128)
	input := "line1\rprogress 50%\r\nline2\n" + largeLine + "\nend"

	var got []string
	readStreamLines(strings.NewReader(input), func(s string) {
		got = append(got, s)
	})

	if len(got) != 6 {
		t.Fatalf("expected 6 segments, got %d", len(got))
	}
	if got[0] != "line1" || got[1] != "progress 50%" || got[2] != "line2" {
		t.Fatalf("unexpected prefix segments: %#v", got[:3])
	}
	if len(got[3]) != maxStreamLineBytes || len(got[4]) != 128 {
		t.Fatalf("unexpected split sizes for >1MiB line: %d and %d", len(got[3]), len(got[4]))
	}
	if got[5] != "end" {
		t.Fatalf("expected trailing line 'end', got %q", got[5])
	}
}

func TestSelectErrorRawPrefersRealStderrError(t *testing.T) {
	stderr := []string{
		"Downloading 12% 1.2MB/10MB",
		"[=====>   ] 45%",
		"Error: unauthorized: authentication required",
	}
	stdout := []string{"Pulling library/nginx"}

	got := selectErrorRaw(stderr, stdout)
	if got != "Error: unauthorized: authentication required" {
		t.Fatalf("unexpected selected error: %q", got)
	}
}

func TestBuildFloorsAndExpandDependencies(t *testing.T) {
	services := map[string]config.Service{
		"web": {
			Image:     "nginx:alpine",
			DependsOn: config.DependsOnConfig{"api": {Condition: "service_started"}},
		},
		"api": {
			Image: "golang:alpine",
			DependsOn: config.DependsOnConfig{
				"db":    {Condition: "service_healthy"},
				"redis": {Condition: "service_started"},
			},
		},
		"db":    {Image: "postgres:alpine"},
		"redis": {Image: "redis:alpine"},
	}

	floors, err := BuildFloors(services)
	if err != nil {
		t.Fatalf("BuildFloors failed: %v", err)
	}
	wantFloors := [][]string{
		{"db", "redis"},
		{"api"},
		{"web"},
	}
	if !reflect.DeepEqual(floors, wantFloors) {
		t.Fatalf("got floors %v, want %v", floors, wantFloors)
	}

	expanded, err := expandServiceDependencies(services, []string{"api"})
	if err != nil {
		t.Fatalf("expandServiceDependencies failed: %v", err)
	}
	wantExpanded := []string{"api", "db", "redis"}
	if !reflect.DeepEqual(expanded, wantExpanded) {
		t.Fatalf("got expanded %v, want %v", expanded, wantExpanded)
	}

	conds := computeWaitConditions(services, false)
	if conds["db"] != "service_healthy" {
		t.Fatalf("expected db to require service_healthy, got %q", conds["db"])
	}
}

func TestParsePullProgress(t *testing.T) {
	p1 := parsePullProgress("Downloading [====>   ] 14.2MB/28.4MB")
	if !p1.HasPct || p1.Percent != 50 {
		t.Fatalf("expected 50%%, got %+v", p1)
	}
	if !strings.Contains(p1.Detail, "14.2MB/28.4MB") {
		t.Fatalf("unexpected detail: %+v", p1)
	}

	p2 := parsePullProgress("Extracting layer 75%")
	if !p2.HasPct || p2.Percent != 75 {
		t.Fatalf("expected 75%%, got %+v", p2)
	}
}

func TestPullTrackerMonotonicMultiLayer(t *testing.T) {
	lines := []string{
		"Usando la etiqueta predeterminada: latest",
		"latest: Pulling from library/nginx",
		"40748add9f4f: Downloading [>                             ] 622kB/29.84MB",
		"7863f34d5b18: Downloading [=>                            ] 2.064MB/34.05MB",
		"9c2c223cd436: Download complete",
		"0e963785aec6: Waiting",
		"cf84a1d9ec18: Waiting",
		"2930fe57ac18: Waiting",
		"96b93a1524c5: Waiting",
		"40748add9f4f: Downloading [===============>              ] 15.0MB/29.84MB",
		"7863f34d5b18: Downloading [==============>               ] 17.0MB/34.05MB",
		"9c2c223cd436: Pull complete",
		"40748add9f4f: Download complete",
		"7863f34d5b18: Download complete",
		"0e963785aec6: Pull complete",
		"cf84a1d9ec18: Pull complete",
		"2930fe57ac18: Pull complete",
		"96b93a1524c5: Pull complete",
		"40748add9f4f: Pull complete",
		"7863f34d5b18: Pull complete",
	}

	tracker := newPullTracker()
	prev := -1
	var last PullProgress

	for _, line := range lines {
		p, ok := tracker.update(line)
		if !ok {
			continue
		}
		if !p.HasPct {
			t.Fatalf("expected HasPct=true for %q, got %+v", line, p)
		}
		if p.Percent < prev {
			t.Fatalf("progress jumped backwards from %d%% to %d%% on line %q", prev, p.Percent, line)
		}
		prev = p.Percent
		last = p
	}

	if last.Percent != 100 {
		t.Fatalf("expected final progress 100%%, got %d%% (%+v)", last.Percent, last)
	}
	if len(last.Layers) != 7 {
		t.Fatalf("expected 7 tracked layers, got %d (%v)", len(last.Layers), last.Layers)
	}
	for i, lvl := range last.Layers {
		if lvl != 8 {
			t.Fatalf("expected layer %d to be complete (8), got %d", i, lvl)
		}
	}
}

func TestBuildContainerRunArgsIncludesAliasesAndFields(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Networks: map[string]config.Network{
			"backend": {},
		},
		Volumes: map[string]config.Volume{
			"data": {},
		},
	}
	svc := config.Service{
		Image:      "postgres:16-alpine",
		WorkingDir: "/var/lib/postgresql",
		User:       "postgres",
		Hostname:   "db-host",
		Privileged: true,
		Init:       true,
		ShmSize:    "256m",
		Volumes:    config.VolumeMounts{{Type: "volume", Source: "data", Target: "/data", ReadOnly: true}},
		Networks: config.ServiceNetworks{
			"backend": {Aliases: []string{"primary-db"}, IPv4Address: "172.20.0.5"},
		},
		Command: config.StringOrSlice{"postgres", "-c", "max_connections=200"},
	}

	args := buildContainerRunArgs(cfg, "db", "demo-db-1", svc, svc.Image, containerRunOverrides{Detach: true})
	joined := strings.Join(args, " ")

	for _, expected := range []string{
		"run -d",
		"--name demo-db-1",
		"-w /var/lib/postgresql",
		"-u postgres",
		"-h db-host",
		"--privileged",
		"--init",
		"--shm-size 256m",
		"-v demo_data:/data:ro",
		"--network demo_backend",
		"--ip 172.20.0.5",
		"--network-alias db",
		"--network-alias primary-db",
		"-l com.wslc.compose.project=demo",
		"-l com.wslc.compose.service=db",
		"postgres:16-alpine postgres -c max_connections=200",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got: %s", expected, joined)
		}
	}
}

func TestMultiNetworkCreateAndConnectArgs(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Networks: map[string]config.Network{
			"frontend": {},
			"backend":  {},
		},
	}
	svc := config.Service{
		Image: "golang:1.27-alpine",
		Ports: []string{"3000:3000"},
		Networks: config.ServiceNetworks{
			"frontend": {Aliases: []string{"public-api"}},
			"backend":  {Aliases: []string{"internal-api"}, IPv4Address: "172.20.0.20"},
		},
	}

	attachments := resolveNetworkAttachments(cfg, "api", svc)
	if len(attachments) != 2 {
		t.Fatalf("expected 2 network attachments, got %d", len(attachments))
	}

	if attachments[0].Network != "demo_backend" || attachments[1].Network != "demo_frontend" {
		t.Fatalf("unexpected attachment order: %+v", attachments)
	}

	createArgs := buildContainerCreateArgs(cfg, "api", "demo-api-1", svc, svc.Image, &attachments[0], containerRunOverrides{Detach: true})
	if len(createArgs) == 0 || createArgs[0] != "create" {
		t.Fatalf("expected create subcommand, got: %v", createArgs)
	}

	networkCount := 0
	for _, arg := range createArgs {
		if arg == "--network" {
			networkCount++
		}
		if arg == "-d" {
			t.Fatalf("create args must not include -d flag: %v", createArgs)
		}
	}
	if networkCount != 1 {
		t.Fatalf("expected exactly 1 --network flag in create args, got %d (%v)", networkCount, createArgs)
	}

	createJoined := strings.Join(createArgs, " ")
	for _, expected := range []string{
		"--name demo-api-1",
		"--network demo_backend",
		"--ip 172.20.0.20",
		"--network-alias api",
		"--network-alias internal-api",
	} {
		if !strings.Contains(createJoined, expected) {
			t.Fatalf("expected create args to contain %q, got: %s", expected, createJoined)
		}
	}
	if strings.Contains(createJoined, "demo_frontend") || strings.Contains(createJoined, "public-api") {
		t.Fatalf("create args must not include secondary network or its aliases: %s", createJoined)
	}

	connectArgs := buildNetworkConnectArgs(attachments[1], "demo-api-1")
	wantConnect := []string{
		"network", "connect",
		"--network-alias", "api",
		"--network-alias", "public-api",
		"demo_frontend",
		"demo-api-1",
	}
	if !reflect.DeepEqual(connectArgs, wantConnect) {
		t.Fatalf("got connect args %v, want %v", connectArgs, wantConnect)
	}
}

func TestParseTableContainersSpanishLocalized(t *testing.T) {
	raw := strings.Join([]string{
		"ID. DE CONTENEDOR   IMAGEN               COMANDO                  FECHA DE CREACIÓN   ESTADO                      PUERTOS                  NOMBRES",
		"90aecf63bb09        nginx:alpine         \"/docker-entrypoint.…\"   hace 31 segundos    Up 30 seconds               127.0.0.1:8080->80/tcp   wslc-compose-web-1",
		"55a228fb359e        golang:1.27-alpine   \"/bin/sh\"                hace 32 segundos    Exited (0) 31 seconds ago                            wslc-compose-api-1",
		"17650f5ce3a0        redis:7-alpine       \"docker-entrypoint.s…\"   hace 33 segundos    Up 32 seconds                                        wslc-compose-redis-1",
		"6c9b3a276969        postgres:16-alpine   \"docker-entrypoint.s…\"   hace 33 segundos    Exited (1) 32 seconds ago                            wslc-compose-db-1",
	}, "\n")

	list := parseTableContainers(raw)
	if len(list) != 4 {
		t.Fatalf("expected 4 containers, got %d (%+v)", len(list), list)
	}

	if list[0].ID != "90aecf63bb09" || list[0].Name != "wslc-compose-web-1" || list[0].Status != "Up 30 seconds" || list[0].Ports != "127.0.0.1:8080->80/tcp" {
		t.Fatalf("unexpected web row: %+v", list[0])
	}
	if list[1].Name != "wslc-compose-api-1" || list[1].Status != "Exited (0) 31 seconds ago" || list[1].Ports != "" {
		t.Fatalf("unexpected api row: %+v", list[1])
	}
	if list[2].Name != "wslc-compose-redis-1" || list[2].Status != "Up 32 seconds" || list[2].Ports != "" {
		t.Fatalf("unexpected redis row: %+v", list[2])
	}
	if list[3].Name != "wslc-compose-db-1" || list[3].Status != "Exited (1) 32 seconds ago" || list[3].Ports != "" {
		t.Fatalf("unexpected db row: %+v", list[3])
	}
}
