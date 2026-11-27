package runtime

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func withMockRuntime(
	t *testing.T,
	runFn func(context.Context, ...string) (string, error),
	streamFn func(context.Context, func(string), ...string) (string, error),
) *lockedBuffer {
	t.Helper()
	origRun := execWSLCFunc
	origStream := execWSLCStreamFunc
	origOut := stdoutWriter

	buf := &lockedBuffer{}
	execWSLCFunc = runFn
	execWSLCStreamFunc = streamFn
	stdoutWriter = buf

	t.Cleanup(func() {
		execWSLCFunc = origRun
		execWSLCStreamFunc = origStream
		stdoutWriter = origOut
	})
	return buf
}

func TestUpForegroundStreamsLogsAndStopsInReverseOrderOnCancel(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
			"api": {
				Image:     "golang:alpine",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_started"}},
			},
		},
	}

	var (
		mu             sync.Mutex
		stopped        []string
		stopCtxHadDead = true
		stopCtxActive  = true
	)
	loggedCh := make(chan string, 2)

	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "stop" {
				mu.Lock()
				stopped = append(stopped, args[len(args)-1])
				if ctx.Err() != nil {
					stopCtxActive = false
				}
				if _, ok := ctx.Deadline(); !ok {
					stopCtxHadDead = false
				}
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			if len(args) < 3 || args[0] != "logs" || args[1] != "-f" {
				return "", errors.New("unexpected stream command")
			}
			switch args[2] {
			case "demo-db-1":
				onLine("database ready")
				loggedCh <- "db"
			case "demo-api-1":
				onLine("server listening")
				loggedCh <- "api"
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Up(ctx, cfg, UpOptions{Detach: false})
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-loggedCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for foreground log streams")
		}
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Up returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for Up to stop after cancel")
	}

	output := outBuf.String()
	finishIdx := strings.Index(output, "[+] Running 3/3")
	attachIdx := strings.Index(output, "Attaching to api, db")
	if finishIdx < 0 || attachIdx < 0 || finishIdx >= attachIdx {
		t.Fatalf("expected progress UI to finish before attaching to logs, got output:\n%s", output)
	}
	for _, want := range []string{
		"api | server listening",
		"db  | database ready",
		"Gracefully stopping...",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !stopCtxActive || !stopCtxHadDead {
		t.Fatalf("expected stop to use fresh shutdown context with deadline (active=%v, hasDeadline=%v)", stopCtxActive, stopCtxHadDead)
	}
	wantStopped := []string{"demo-api-1", "demo-db-1"}
	if !reflect.DeepEqual(stopped, wantStopped) {
		t.Fatalf("expected reverse dependency stop order %v, got %v", wantStopped, stopped)
	}
}

func TestUpForegroundShortLivedServiceKeepsStreamingOthersUntilCancel(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"migrator": {Image: "alpine:latest"},
			"web":      {Image: "nginx:alpine"},
		},
	}

	var (
		mu      sync.Mutex
		stopped = make(map[string]bool)
	)
	migratorDone := make(chan struct{})
	webSecondLine := make(chan struct{})

	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "stop" {
				mu.Lock()
				stopped[args[len(args)-1]] = true
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			switch args[len(args)-1] {
			case "demo-migrator-1":
				onLine("migration complete")
				close(migratorDone)
				return "", nil
			case "demo-web-1":
				onLine("web booting")
				select {
				case <-migratorDone:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				onLine("web ready")
				close(webSecondLine)
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "", nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Up(ctx, cfg)
	}()

	select {
	case <-webSecondLine:
	case <-time.After(5 * time.Second):
		t.Fatalf("expected web service to keep streaming logs after migrator exited")
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Up returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for Up to finish")
	}

	output := outBuf.String()
	for _, want := range []string{
		"migrator | migration complete",
		"web      | web booting",
		"web      | web ready",
		"Gracefully stopping...",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !stopped["demo-migrator-1"] || !stopped["demo-web-1"] {
		t.Fatalf("expected all started containers to be stopped on interrupt, got: %v", stopped)
	}
}

func TestUpForegroundAllShortLivedServicesExitCleanly(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"job1": {Image: "alpine:latest"},
			"job2": {Image: "alpine:latest"},
		},
	}

	var (
		mu        sync.Mutex
		stopCalls int
	)

	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "stop" {
				mu.Lock()
				stopCalls++
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			onLine("finished")
			return "", nil
		},
	)

	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up returned unexpected error: %v", err)
	}

	output := outBuf.String()
	for _, want := range []string{"job1 | finished", "job2 | finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Gracefully stopping...") {
		t.Fatalf("did not expect graceful stop banner when all services exited naturally, got:\n%s", output)
	}

	mu.Lock()
	defer mu.Unlock()
	if stopCalls != 0 {
		t.Fatalf("expected no stop calls when all services exited naturally, got %d", stopCalls)
	}
}

func TestUpDetachedSkipsForegroundStreaming(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}

	var (
		mu          sync.Mutex
		streamCalls int
		stopCalls   int
	)

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "stop" {
				mu.Lock()
				stopCalls++
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			mu.Lock()
			streamCalls++
			mu.Unlock()
			return "", nil
		},
	)

	if err := Up(context.Background(), cfg, UpOptions{Detach: true}); err != nil {
		t.Fatalf("detached Up returned unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if streamCalls != 0 || stopCalls != 0 {
		t.Fatalf("expected detached mode to skip logs and stop, got streamCalls=%d stopCalls=%d", streamCalls, stopCalls)
	}
}

func TestUpForegroundStartupErrorPreservesRollback(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
			"api": {
				Image:     "golang:alpine",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_started"}},
			},
		},
	}

	var (
		mu          sync.Mutex
		removed     []string
		streamCalls int
	)

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "run" {
				if strings.Contains(strings.Join(args, " "), "demo-api-1") {
					return "", errors.New("start failed")
				}
			}
			if len(args) > 0 && args[0] == "remove" {
				mu.Lock()
				removed = append(removed, args[len(args)-1])
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			mu.Lock()
			streamCalls++
			mu.Unlock()
			return "", nil
		},
	)

	err := Up(context.Background(), cfg, UpOptions{Detach: false})
	if err == nil {
		t.Fatalf("expected startup error from Up")
	}

	mu.Lock()
	defer mu.Unlock()
	if streamCalls != 0 {
		t.Fatalf("expected no log streaming when startup fails, got %d", streamCalls)
	}
	dbRemovedCount := 0
	for _, name := range removed {
		if name == "demo-db-1" {
			dbRemovedCount++
		}
	}
	if dbRemovedCount < 2 {
		t.Fatalf("expected rollback to remove started db container after api startup failure, got removals: %v", removed)
	}
}

func extractContainerNameFromRunArgs(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--name" {
			return args[i+1]
		}
	}
	return ""
}

func TestUpNoDepsStartsOnlyTargetService(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db":    {Image: "postgres:16-alpine"},
			"redis": {Image: "redis:7-alpine"},
			"api": {
				Image: "golang:alpine",
				DependsOn: config.DependsOnConfig{
					"db":    {Condition: "service_healthy"},
					"redis": {Condition: "service_started"},
				},
			},
			"web": {
				Image:     "nginx:alpine",
				DependsOn: config.DependsOnConfig{"api": {Condition: "service_started"}},
			},
		},
	}

	var (
		mu      sync.Mutex
		started []string
	)

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "run" {
				mu.Lock()
				started = append(started, extractContainerNameFromRunArgs(args))
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	err := Up(context.Background(), cfg, UpOptions{
		Detach:   true,
		NoDeps:   true,
		Services: []string{"web"},
	})
	if err != nil {
		t.Fatalf("Up with NoDeps returned unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantStarted := []string{"demo-web-1"}
	if !reflect.DeepEqual(started, wantStarted) {
		t.Fatalf("expected only target service %v to start, got %v", wantStarted, started)
	}
}

func TestUpNoDepsMultipleTargetsRespectsInternalOrderAndSkipsExcludedDeps(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
			"api": {
				Image:     "golang:alpine",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_healthy"}},
			},
			"web": {
				Image:     "nginx:alpine",
				DependsOn: config.DependsOnConfig{"api": {Condition: "service_started"}},
			},
		},
	}

	var (
		mu      sync.Mutex
		started []string
	)

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "run" {
				mu.Lock()
				started = append(started, extractContainerNameFromRunArgs(args))
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	err := Up(context.Background(), cfg, UpOptions{
		Detach:   true,
		NoDeps:   true,
		Services: []string{"web", "api"},
	})
	if err != nil {
		t.Fatalf("Up with NoDeps for multiple targets returned unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantStarted := []string{"demo-api-1", "demo-web-1"}
	if !reflect.DeepEqual(started, wantStarted) {
		t.Fatalf("expected %v in dependency order without excluded db, got %v", wantStarted, started)
	}
}

func TestRunNoDepsBehavior(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
			"api": {
				Image:     "golang:alpine",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_started"}},
			},
		},
	}

	t.Run("no-deps skips linked services", func(t *testing.T) {
		var (
			mu      sync.Mutex
			started []string
		)

		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "run" {
					mu.Lock()
					started = append(started, extractContainerNameFromRunArgs(args))
					mu.Unlock()
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		err := Run(context.Background(), cfg, "api", []string{"echo", "ok"}, RunOptions{
			NoDeps: true,
			Detach: true,
		})
		if err != nil {
			t.Fatalf("Run with NoDeps returned unexpected error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(started) != 1 || !strings.HasPrefix(started[0], "demo-api-run-") {
			t.Fatalf("expected only one-off api container to run, got %v", started)
		}
	})

	t.Run("default starts linked services first", func(t *testing.T) {
		var (
			mu      sync.Mutex
			started []string
		)

		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "inspect" {
					return "", ErrNotFound
				}
				if len(args) > 0 && args[0] == "run" {
					mu.Lock()
					started = append(started, extractContainerNameFromRunArgs(args))
					mu.Unlock()
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		err := Run(context.Background(), cfg, "api", []string{"echo", "ok"}, RunOptions{
			NoDeps: false,
			Detach: true,
		})
		if err != nil {
			t.Fatalf("Run without NoDeps returned unexpected error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(started) != 2 || started[0] != "demo-db-1" || !strings.HasPrefix(started[1], "demo-api-run-") {
			t.Fatalf("expected db to start before one-off api container, got %v", started)
		}
	})
}

func TestCreateVolumeArgsDriverOptsLabelsAndExternal(t *testing.T) {
	t.Run("local driver omits -d while preserving driver_opts and labels", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "demo_cache", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		vol := config.Volume{
			Driver: "local",
			DriverOpts: map[string]string{
				"type":   "tmpfs",
				"o":      "size=100m,uid=1000",
				"device": "tmpfs",
			},
			Labels: config.LabelMap{
				"z.key": "last",
				"a.key": "first",
			},
		}

		res, created, err := createVolume(context.Background(), "demo_cache", vol)
		if err != nil {
			t.Fatalf("createVolume failed: %v", err)
		}
		if !created || res != "demo_cache" {
			t.Fatalf("expected created=true res='demo_cache', got created=%v res=%q", created, res)
		}

		wantArgs := []string{
			"volume", "create",
			"-o", "device=tmpfs",
			"-o", "o=size=100m,uid=1000",
			"-o", "type=tmpfs",
			"-l", "a.key=first",
			"-l", "z.key=last",
			"demo_cache",
		}
		if !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("got volume create args %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("explicit vhd driver passes -d vhd", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "demo_vhd", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		vol := config.Volume{
			Driver: "vhd",
			DriverOpts: map[string]string{
				"size": "10G",
			},
		}

		res, created, err := createVolume(context.Background(), "demo_vhd", vol)
		if err != nil || !created || res != "demo_vhd" {
			t.Fatalf("expected vhd volume creation to succeed, got created=%v res=%q err=%v", created, res, err)
		}

		wantArgs := []string{
			"volume", "create",
			"-d", "vhd",
			"-o", "size=10G",
			"demo_vhd",
		}
		if !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("got volume create args %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("explicit volume name overrides prefixed name", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "exact_vol_name", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		vol := config.Volume{Name: "exact_vol_name"}
		_, created, err := createVolume(context.Background(), "demo_ignored", vol)
		if err != nil || !created {
			t.Fatalf("expected volume creation to succeed, got created=%v err=%v", created, err)
		}

		wantArgs := []string{"volume", "create", "exact_vol_name"}
		if !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("got volume create args %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("external volume skips creation", func(t *testing.T) {
		called := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				called = true
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		vol := config.Volume{
			External:   true,
			Driver:     "local",
			DriverOpts: map[string]string{"type": "nfs"},
			Labels:     config.LabelMap{"env": "prod"},
		}

		res, created, err := createVolume(context.Background(), "ext_data", vol)
		if err != nil {
			t.Fatalf("expected no error for external volume, got: %v", err)
		}
		if called || created || res != "ext_data" {
			t.Fatalf("expected external volume to skip creation, got called=%v created=%v res=%q", called, created, res)
		}
	})

	t.Run("already exists volume is idempotent", func(t *testing.T) {
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				return "", ErrAlreadyExists
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		res, created, err := createVolume(context.Background(), "demo_data", config.Volume{Driver: "local"})
		if err != nil {
			t.Fatalf("expected nil error on ErrAlreadyExists, got: %v", err)
		}
		if created || res != "demo_data" {
			t.Fatalf("expected created=false res='demo_data', got created=%v res=%q", created, res)
		}
	})
}

func TestUpSkipsRecreateWhenRunningUpToDateAndHandlesCompletedDep(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"init": {Image: "alpine:latest"},
			"web": {
				Image:     "nginx:alpine",
				DependsOn: config.DependsOnConfig{"init": {Condition: "service_completed_successfully"}},
			},
		},
	}

	t.Run("skips recreating up-to-date running container and exited-0 completed dep", func(t *testing.T) {
		var (
			mu       sync.Mutex
			runCalls []string
		)
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "inspect" {
					target := args[len(args)-1]
					if target == "demo-init-1" {
						return "exited||0|alpine:latest|", nil
					}
					if target == "demo-web-1" {
						return "running||0|nginx:alpine|", nil
					}
				}
				if len(args) > 0 && (args[0] == "run" || args[0] == "start") {
					mu.Lock()
					runCalls = append(runCalls, strings.Join(args, " "))
					mu.Unlock()
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := Up(context.Background(), cfg, UpOptions{Detach: true}); err != nil {
			t.Fatalf("Up failed: %v", err)
		}
		if len(runCalls) != 0 {
			t.Fatalf("expected no container recreation/relaunch, got calls: %v", runCalls)
		}
	})

	t.Run("recreates when image changes or force-recreate is set", func(t *testing.T) {
		var (
			mu      sync.Mutex
			started []string
		)
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "inspect" {
					target := args[len(args)-1]
					if target == "demo-init-1" {
						return "exited||0|alpine:latest|", nil
					}
					if target == "demo-web-1" {
						return "running||0|nginx:1.24-alpine|", nil
					}
				}
				if len(args) > 0 && args[0] == "run" {
					mu.Lock()
					started = append(started, extractContainerNameFromRunArgs(args))
					mu.Unlock()
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := Up(context.Background(), cfg, UpOptions{Detach: true}); err != nil {
			t.Fatalf("Up failed: %v", err)
		}
		if !reflect.DeepEqual(started, []string{"demo-web-1"}) {
			t.Fatalf("expected only changed web container recreated, got: %v", started)
		}
	})
}

func TestStartAndRestartRespectDependsOnOrderAndHealth(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {
				Image: "postgres:16-alpine",
				HealthCheck: &config.HealthCheck{
					Test:     config.HealthCheckTest{"CMD", "pg_isready"},
					Interval: "5ms",
					Retries:  2,
				},
			},
			"api": {
				Image:     "golang:alpine",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_healthy"}},
			},
		},
	}

	var (
		mu     sync.Mutex
		events []string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) >= 2 && args[0] == "inspect" {
				return "running||0", nil
			}
			if len(args) >= 2 && args[0] == "exec" {
				events = append(events, "probe:"+args[1])
				return "ok", nil
			}
			if len(args) >= 2 && (args[0] == "start" || args[0] == "stop") {
				events = append(events, args[0]+":"+args[len(args)-1])
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Start(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	wantStart := []string{"start:demo-db-1", "probe:demo-db-1", "start:demo-api-1"}
	if !reflect.DeepEqual(events, wantStart) {
		t.Fatalf("got start sequence %v, want %v", events, wantStart)
	}

	events = nil
	if err := Restart(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	wantRestart := []string{"stop:demo-api-1", "stop:demo-db-1", "start:demo-db-1", "probe:demo-db-1", "start:demo-api-1"}
	if !reflect.DeepEqual(events, wantRestart) {
		t.Fatalf("got restart sequence %v, want %v", events, wantRestart)
	}
}

func TestRunSkipsRestartPolicyAndClearsCommandOnEntrypointOverride(t *testing.T) {
	cfg := config.ComposeFile{Name: "demo"}
	svc := config.Service{
		Image:   "alpine:latest",
		Restart: "always",
		Command: config.StringOrSlice{"server", "--port", "8080"},
	}

	args := buildContainerRunArgs(cfg, "api", "demo-api-run-1", svc, svc.Image, containerRunOverrides{
		SkipRestart:   true,
		HasEntrypoint: true,
		Entrypoint:    "/bin/sh",
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--restart") {
		t.Fatalf("one-off container must not inherit restart policy: %s", joined)
	}
	if strings.Contains(joined, "server") || strings.Contains(joined, "--port") {
		t.Fatalf("entrypoint override without command must clear service.Command: %s", joined)
	}
	if !strings.Contains(joined, "--entrypoint /bin/sh") {
		t.Fatalf("expected --entrypoint /bin/sh in args: %s", joined)
	}
}

func TestScanStreamLinesPreservesIndentationAndEmptyLinesAndForegroundPropagatesLogsError(t *testing.T) {
	var got []string
	scanStreamLines(strings.NewReader("  indented\n\n\tsecond\n"), func(s string) {
		got = append(got, s)
	})
	want := []string{"  indented", "", "\tsecond"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got scanned lines %#v, want %#v", got, want)
	}

	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", errors.New("log stream broken")
		},
	)
	err := Up(context.Background(), cfg, UpOptions{Detach: false})
	if err == nil || !strings.Contains(err.Error(), "log stream broken") {
		t.Fatalf("expected foreground Up to propagate Logs error, got: %v", err)
	}
}

func TestUpPropagatesOrphanRemovalError(t *testing.T) {
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "ps" {
				return "111\tdemo-old-1\talpine\tUp 5s\t\tcom.wslc.compose.project=demo,com.wslc.compose.service=old", nil
			}
			if len(args) > 0 && args[0] == "remove" && args[len(args)-1] == "demo-old-1" {
				return "", errors.New("permission denied removing orphan")
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}
	err := Up(context.Background(), cfg, UpOptions{Detach: true, RemoveOrphans: true})
	if err == nil || !strings.Contains(err.Error(), "permission denied removing orphan") {
		t.Fatalf("expected Up to propagate orphan removal error, got: %v", err)
	}
}
