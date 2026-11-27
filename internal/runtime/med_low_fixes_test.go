package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"wslc-compose/internal/config"
)

func TestDownRMIRespectsDisabledProfilesWhileCleaningProjectContainers(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
			"debug": {
				Image:    "busybox:debug",
				Profiles: []string{"debug"},
			},
			"dbgbuild": {
				Build:    config.BuildConfig{Context: "./dbg"},
				Profiles: []string{"debug"},
			},
		},
	}

	if got := selectRMIImages(cfg, "all"); !reflect.DeepEqual(got, []string{"nginx:alpine"}) {
		t.Fatalf("selectRMIImages without active profiles got %v, want [nginx:alpine]", got)
	}

	cfgWithDebug := cfg
	cfgWithDebug.ActiveProfiles = []string{"debug"}
	wantDebug := []string{"demo-dbgbuild:latest", "busybox:debug", "nginx:alpine"}
	gotDebug := selectRMIImages(cfgWithDebug, "all")
	wantSet := map[string]bool{"demo-dbgbuild:latest": true, "busybox:debug": true, "nginx:alpine": true}
	if len(gotDebug) != 3 {
		t.Fatalf("selectRMIImages with debug profile got %v, want %v", gotDebug, wantDebug)
	}
	for _, img := range gotDebug {
		if !wantSet[img] {
			t.Fatalf("unexpected image %q in %v", img, gotDebug)
		}
	}

	var (
		mu         sync.Mutex
		removedCtr []string
		removedImg []string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) > 0 && args[0] == "remove" {
				removedCtr = append(removedCtr, args[len(args)-1])
			}
			if len(args) > 0 && args[0] == "rmi" {
				removedImg = append(removedImg, args[len(args)-1])
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Down(context.Background(), cfg, DownOptions{RMI: "all"}); err != nil {
		t.Fatalf("Down failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	ctrSet := make(map[string]bool)
	for _, c := range removedCtr {
		ctrSet[c] = true
	}
	if !ctrSet["demo-web-1"] || !ctrSet["demo-debug-1"] || !ctrSet["demo-dbgbuild-1"] {
		t.Fatalf("expected Down to still clean up containers of all services, got %v", removedCtr)
	}
	if !reflect.DeepEqual(removedImg, []string{"nginx:alpine"}) {
		t.Fatalf("expected Down --rmi all without active profiles to remove only [nginx:alpine], got %v", removedImg)
	}
}

func TestResolveWSLCBinaryFindsConsistentExecutable(t *testing.T) {
	dir := t.TempDir()
	name := "wslc"
	content := []byte("#!/bin/sh\nexit 0\n")
	if goruntime.GOOS == "windows" {
		name = "wslc.cmd"
		content = []byte("@echo off\r\necho shim-ok\r\n")
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, content, 0755); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	t.Setenv("PATH", dir)
	resolved, err := ResolveWSLCBinary()
	if err != nil || resolved == "" {
		t.Fatalf("ResolveWSLCBinary failed: path=%q err=%v", resolved, err)
	}

	if goruntime.GOOS == "windows" {
		out, runErr := runWSLC(context.Background(), "version")
		if runErr != nil || out != "shim-ok" {
			t.Fatalf("runWSLC failed using resolved shim: out=%q err=%v", out, runErr)
		}
	}
}

func TestExplicitVolumeTypeNotClassifiedAsBindMount(t *testing.T) {
	cfg := config.ComposeFile{
		Name:    "demo",
		BaseDir: t.TempDir(),
	}
	mount := config.VolumeMount{
		Type:   "volume",
		Source: "undeclared_vol",
		Target: "/etc/nginx/conf.d/default.conf",
	}
	if hostPath, isBind := resolveBindMountSource(cfg, mount); isBind || hostPath != "" {
		t.Fatalf("type: volume must not be classified as bind mount, got path=%q isBind=%v", hostPath, isBind)
	}
}

func TestConfigHashIncludesTopLevelNamesAndUpRecreatesOnImageIDChange(t *testing.T) {
	svc := config.Service{
		Image:    "myapp:latest",
		Volumes:  config.VolumeMounts{{Type: "volume", Source: "data", Target: "/data"}},
		Networks: config.ServiceNetworks{"back": {}},
	}
	cfgA := config.ComposeFile{
		Name:     "demo",
		Volumes:  map[string]config.Volume{"data": {Name: "vol_v1"}},
		Networks: map[string]config.Network{"back": {Name: "net_v1"}},
	}
	cfgB := config.ComposeFile{
		Name:     "demo",
		Volumes:  map[string]config.Volume{"data": {Name: "vol_v2"}},
		Networks: map[string]config.Network{"back": {Name: "net_v1"}},
	}
	cfgC := config.ComposeFile{
		Name:     "demo",
		Volumes:  map[string]config.Volume{"data": {Name: "vol_v1"}},
		Networks: map[string]config.Network{"back": {Name: "net_v2"}},
	}

	hashA := computeServiceConfigHash(cfgA, "app", svc, svc.Image)
	hashB := computeServiceConfigHash(cfgB, "app", svc, svc.Image)
	hashC := computeServiceConfigHash(cfgC, "app", svc, svc.Image)
	if hashA == hashB {
		t.Fatalf("changing top-level volume name did not change config hash: %s", hashA)
	}
	if hashA == hashC {
		t.Fatalf("changing top-level network name did not change config hash: %s", hashA)
	}

	upCfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"app": svc,
		},
		Volumes:  cfgA.Volumes,
		Networks: cfgA.Networks,
	}
	expectedHash := computeServiceConfigHash(withDefaultNetwork(upCfg, upCfg.Services), "app", svc, svc.Image)

	var (
		mu      sync.Mutex
		started []string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) >= 2 && args[0] == "inspect" {
				return "running||0|myapp:latest|" + expectedHash + "|sha256:111111111111aaaa", nil
			}
			if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				return "sha256:222222222222bbbb", nil
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

	if err := Up(context.Background(), upCfg, UpOptions{Detach: true}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if !reflect.DeepEqual(started, []string{"demo-app-1"}) {
		t.Fatalf("expected container recreation when local image ID changed, got started=%v", started)
	}
}

func TestRestartSkipsUncreatedServiceWithoutLeavingActiveOnesStopped(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db":     {Image: "postgres:16-alpine"},
			"worker": {Image: "alpine:latest", DependsOn: config.DependsOnConfig{"db": {Condition: "service_started"}}},
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
				if args[len(args)-1] == "demo-worker-1" {
					return "", ErrNotFound
				}
				return "running||0|postgres:16-alpine|", nil
			}
			if len(args) >= 2 && (args[0] == "stop" || args[0] == "start") {
				events = append(events, args[0]+":"+args[len(args)-1])
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Restart(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Restart failed when worker was never created: %v", err)
	}
	want := []string{"stop:demo-db-1", "start:demo-db-1"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("got events %v, want %v", events, want)
	}
}

func TestDownRemoveVolumesPropagatesDashVToProjectAndOrphanContainers(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}

	var (
		mu          sync.Mutex
		removeCalls [][]string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "ps" {
				return "999\tdemo-orphan-1\talpine\tUp 3s\t\tcom.wslc.compose.project=demo,com.wslc.compose.service=orphan", nil
			}
			if len(args) > 0 && args[0] == "remove" {
				mu.Lock()
				removeCalls = append(removeCalls, append([]string(nil), args...))
				mu.Unlock()
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Down(context.Background(), cfg, DownOptions{RemoveVolumes: true, RemoveOrphans: true}); err != nil {
		t.Fatalf("Down failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(removeCalls) != 2 {
		t.Fatalf("expected 2 remove calls, got %v", removeCalls)
	}
	for _, call := range removeCalls {
		hasV := false
		for _, a := range call {
			if a == "-v" {
				hasV = true
			}
		}
		if !hasV {
			t.Fatalf("expected -v in remove call, got %v", call)
		}
	}
}

func TestRunDetachedAutoRemoveMultiNetworkIncludesRmInCreate(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Networks: map[string]config.Network{
			"front": {},
			"back":  {},
		},
		Services: map[string]config.Service{
			"api": {
				Image: "alpine:latest",
				Networks: config.ServiceNetworks{
					"front": {},
					"back":  {},
				},
			},
		},
	}

	var createArgs []string
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "create" {
				createArgs = append([]string(nil), args...)
				return "ctr-rm-1", nil
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Run(context.Background(), cfg, "api", []string{"echo", "hi"}, RunOptions{Remove: true, Detach: true}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	hasRM := false
	for _, a := range createArgs {
		if a == "--rm" {
			hasRM = true
			break
		}
	}
	if !hasRM {
		t.Fatalf("expected create args to include --rm, got %v", createArgs)
	}
}

func TestBuildImageArgsPreservesPOSIXContextAndJoinsRelativeDockerfile(t *testing.T) {
	args := buildImageArgs("demo:latest", config.BuildConfig{
		Context:    "/mnt/c/projects/demo/../app",
		Dockerfile: "docker/Dockerfile.dev",
	}, BuildOptions{
		ProjectDir: `C:\Users\dev\project`,
	})

	wantCtx := "/mnt/c/projects/app"
	wantDF := "/mnt/c/projects/app/docker/Dockerfile.dev"
	if args[len(args)-1] != wantCtx {
		t.Fatalf("got build context %q, want %q", args[len(args)-1], wantCtx)
	}
	gotDF := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" {
			gotDF = args[i+1]
		}
	}
	if gotDF != wantDF {
		t.Fatalf("got dockerfile %q, want %q", gotDF, wantDF)
	}
}

func TestImagesDigestMatchingAndDynamicHeaderTableParsing(t *testing.T) {
	table := strings.Join([]string{
		"REPOSITORY   TAG       DIGEST                    IMAGE ID       CREATED        PLATFORM       SIZE",
		"nginx        <none>    sha256:abcdef1234567890   90aecf63bb09   2 hours ago    linux/amd64    67.5MB",
		"redis        7-alpine  sha256:fedcba0987654321   17650f5ce3a0   3 hours ago    linux/amd64    41.2MB",
	}, "\n")

	parsed := parseImagesTable(table)
	if len(parsed) != 2 {
		t.Fatalf("expected 2 parsed images, got %d", len(parsed))
	}
	if parsed[0].Digest != "sha256:abcdef1234567890" || parsed[0].ID != "90aecf63bb09" || parsed[0].Size != "67.5MB" {
		t.Fatalf("unexpected row 0: %+v", parsed[0])
	}
	if !imageInList(table, "nginx@sha256:abcdef1234567890") {
		t.Fatalf("expected digest ref to match imageInList")
	}
	if !imageInList(table, "redis:7-alpine@sha256:fedcba0987654321") {
		t.Fatalf("expected tag+digest ref to match imageInList")
	}

	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "images" {
				for _, a := range args {
					if a == "--format" {
						return "{{.Repository}}", nil
					}
				}
				return table, nil
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web":   {Image: "nginx@sha256:abcdef1234567890"},
			"cache": {Image: "redis:7-alpine@sha256:fedcba0987654321"},
		},
	}
	if err := Images(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Images failed: %v", err)
	}
	output := outBuf.String()
	if !strings.Contains(output, "90aecf63bb09") || !strings.Contains(output, "67.5MB") {
		t.Fatalf("expected matched nginx digest image ID and size in output:\n%s", output)
	}
	if !strings.Contains(output, "17650f5ce3a0") || !strings.Contains(output, "41.2MB") {
		t.Fatalf("expected matched redis tag+digest image ID and size in output:\n%s", output)
	}
}

func TestShutdownTimeoutScalesWithStopGracePeriodFloors(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db":  {Image: "postgres:16", StopGracePeriod: "45s"},
			"api": {Image: "myapp:latest", StopGracePeriod: "30s"},
		},
	}
	floors := [][]string{{"db"}, {"api"}}

	got := computeShutdownTimeout(cfg, floors, nil)
	if got < 75*time.Second {
		t.Fatalf("expected shutdown timeout >= 75s for 45s+30s floors, got %v", got)
	}

	smallCfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine", StopGracePeriod: "500ms"},
		},
	}
	minGot := computeShutdownTimeout(smallCfg, [][]string{{"web"}}, nil)
	if minGot != 30*time.Second {
		t.Fatalf("expected minimum 30s timeout, got %v", minGot)
	}
}

func TestLogsPreservesSGRColorsWhileStandardStreamStripsANSI(t *testing.T) {
	colored := "\x1b[31mERROR:\x1b[0m failed \x1b[2Jafter clear"
	if got := stripNonSGRANSI(colored); got != "\x1b[31mERROR:\x1b[0m failed after clear" {
		t.Fatalf("stripNonSGRANSI got %q", got)
	}
	if got := stripANSI(colored); got != "ERROR: failed after clear" {
		t.Fatalf("stripANSI got %q", got)
	}

	var sawPreserve bool
	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			sawPreserve = shouldPreserveSGR(ctx)
			onLine("\x1b[32mREADY\x1b[0m")
			return "", nil
		},
	)

	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"app": {Image: "alpine:latest"},
		},
	}
	if err := Logs(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	if !sawPreserve {
		t.Fatalf("expected Logs to enable SGR preservation on stream context")
	}
	if !strings.Contains(outBuf.String(), "\x1b[32mREADY\x1b[0m") {
		t.Fatalf("expected ANSI SGR sequence preserved in Logs output, got %q", outBuf.String())
	}
}
