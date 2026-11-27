package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"wslc-compose/internal/config"
)

func TestAuditBuildFloorsCyclesAndSelfDependency(t *testing.T) {
	t.Run("self dependency", func(t *testing.T) {
		_, err := BuildFloors(map[string]config.Service{
			"a": {Image: "alpine", DependsOn: config.DependsOnConfig{"a": {Condition: "service_started"}}},
		})
		if err == nil || !strings.Contains(err.Error(), "cannot depend on itself") {
			t.Fatalf("expected self-dependency error, got %v", err)
		}
	})

	t.Run("circular dependency", func(t *testing.T) {
		_, err := BuildFloors(map[string]config.Service{
			"a": {Image: "alpine", DependsOn: config.DependsOnConfig{"b": {Condition: "service_started"}}},
			"b": {Image: "alpine", DependsOn: config.DependsOnConfig{"a": {Condition: "service_started"}}},
		})
		if err == nil || !strings.Contains(err.Error(), "circular dependency") {
			t.Fatalf("expected circular dependency error, got %v", err)
		}
	})
}

func TestAuditBugFindOrphanContainersCrossProjectAndProfiles(t *testing.T) {
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "ps" {
				return strings.Join([]string{
					"111\tapp-web-1\tnginx:alpine\tUp 10s\t\tcom.wslc.compose.project=app,com.wslc.compose.service=web",
					"222\tapp-staging-db-1\tpostgres:16\tUp 10s\t\tcom.wslc.compose.project=app-staging,com.wslc.compose.service=db",
					"333\tapp-debug-1\talpine:latest\tUp 10s\t\tcom.wslc.compose.project=app,com.wslc.compose.service=debug",
					"444\tapp-removed-1\talpine:latest\tUp 10s\t\tcom.wslc.compose.project=app,com.wslc.compose.service=removed",
				}, "\n"), nil
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	fullCfg := config.ComposeFile{
		Name: "app",
		Services: map[string]config.Service{
			"web":   {Image: "nginx:alpine"},
			"debug": {Image: "alpine:latest", Profiles: []string{"debug"}},
		},
	}
	cfg := fullCfg.FilterByProfiles(nil, nil)

	orphans := findOrphanContainers(context.Background(), cfg)
	if len(orphans) != 1 || orphans[0].Name != "app-removed-1" {
		t.Fatalf("expected only app-removed-1 as orphan, got: %+v", orphans)
	}
}

func TestAuditBugRunMultiNetworkRemoveAndDetachKillsContainer(t *testing.T) {
	var (
		mu      sync.Mutex
		removed []string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "create" {
				return "ctr-123", nil
			}
			if len(args) > 0 && args[0] == "remove" {
				mu.Lock()
				removed = append(removed, args[len(args)-1])
				mu.Unlock()
			}
			return "ok", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

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

	if err := Run(context.Background(), cfg, "api", []string{"sleep", "60"}, RunOptions{Remove: true, Detach: true}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(removed) > 0 {
		t.Fatalf("detached multi-network container was immediately killed and removed: %v", removed)
	}
}

func TestAuditBugRunIgnoresDependsOnHealthCondition(t *testing.T) {
	var (
		mu            sync.Mutex
		healthProbed  bool
		apiRanAtCount int
		probeCount    int
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) >= 2 && args[0] == "inspect" {
				return "running||0", nil
			}
			if len(args) >= 2 && args[0] == "exec" && args[1] == "demo-db-1" {
				healthProbed = true
				probeCount++
				return "ok", nil
			}
			if len(args) > 0 && args[0] == "run" && strings.Contains(strings.Join(args, " "), "demo-api-run-") {
				apiRanAtCount = probeCount
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
			"db": {
				Image: "postgres:16-alpine",
				HealthCheck: &config.HealthCheck{
					Test:     config.HealthCheckTest{"CMD", "pg_isready"},
					Interval: "10ms",
					Retries:  2,
				},
			},
			"api": {
				Image:     "alpine:latest",
				DependsOn: config.DependsOnConfig{"db": {Condition: "service_healthy"}},
			},
		},
	}

	if err := Run(context.Background(), cfg, "api", []string{"true"}, RunOptions{Detach: true}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !healthProbed || apiRanAtCount == 0 {
		t.Fatalf("Run started api without waiting for db service_healthy condition")
	}
}

func TestAuditBugDownSkipsGracefulStopByDefault(t *testing.T) {
	var (
		mu       sync.Mutex
		commands []string
	)
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			mu.Lock()
			if len(args) > 0 {
				commands = append(commands, args[0])
			}
			mu.Unlock()
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
		},
	}

	if err := Down(context.Background(), cfg); err != nil {
		t.Fatalf("Down failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) < 2 || commands[0] != "stop" || commands[1] != "remove" {
		t.Fatalf("expected graceful stop before remove, got command sequence: %v", commands)
	}
}

func TestAuditBugClassifyWSLCErrorTruncatesAndMisclassifies(t *testing.T) {
	err := classifyWSLCError("40748add9f4f: Already exists\nError: pull access denied", errors.New("exit 1"))
	if errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("pull denial error was misclassified as ErrAlreadyExists: %v", err)
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("real error line was truncated: %v", err)
	}
}

func TestAuditBugHealthcheckNoneTimeoutAndCmdQuoting(t *testing.T) {
	t.Run("NONE test must not run exec NONE", func(t *testing.T) {
		execCalled := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					return "running||0", nil
				}
				if len(args) > 0 && args[0] == "exec" {
					execCalled = true
					return "", errors.New("executable file not found: NONE")
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		svc := config.Service{
			Image: "nginx:alpine",
			HealthCheck: &config.HealthCheck{
				Test:     config.HealthCheckTest{"NONE"},
				Interval: "5ms",
				Retries:  1,
			},
		}
		if err := WaitForCondition(context.Background(), "demo-web-1", svc, "service_healthy"); err != nil {
			t.Fatalf("WaitForCondition failed on NONE healthcheck: %v", err)
		}
		if execCalled {
			t.Fatalf("exec must not be called when healthcheck test is NONE")
		}
	})

	t.Run("formatHealthCmd preserves quoting for CMD args with spaces", func(t *testing.T) {
		got := formatHealthCmd(config.HealthCheckTest{"CMD", "sh", "-c", "test -f /ready && echo ok"})
		if got == "sh -c test -f /ready && echo ok" {
			t.Fatalf("formatHealthCmd dropped argument quoting: %q", got)
		}
	})

	t.Run("waitForHealthy honors healthcheck timeout", func(t *testing.T) {
		probeSawDeadline := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					return "running||0", nil
				}
				if len(args) > 0 && args[0] == "exec" {
					_, probeSawDeadline = ctx.Deadline()
					return "ok", nil
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)
		svc := config.Service{
			Image: "nginx:alpine",
			HealthCheck: &config.HealthCheck{
				Test:     config.HealthCheckTest{"CMD", "true"},
				Interval: "5ms",
				Timeout:  "2s",
				Retries:  1,
			},
		}
		if err := WaitForCondition(context.Background(), "demo-web-1", svc, "service_healthy"); err != nil {
			t.Fatalf("WaitForCondition failed: %v", err)
		}
		if !probeSawDeadline {
			t.Fatalf("expected healthcheck probe context to have timeout deadline")
		}
	})
}

func TestAuditBugBareRunEnvFlagOverriddenByServiceEnvironment(t *testing.T) {
	cfg := config.ComposeFile{Name: "demo"}
	svc := config.Service{
		Image: "alpine:latest",
		Environment: config.EnvironmentMap{
			"APP_ENV": "compose_default",
		},
	}
	args := buildContainerRunArgs(cfg, "api", "demo-api-run-1", svc, svc.Image, containerRunOverrides{
		Env: []string{"APP_ENV"},
	})

	bareIdx := -1
	defaultIdx := -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" && args[i+1] == "APP_ENV" {
			bareIdx = i
		}
		if args[i] == "-e" && args[i+1] == "APP_ENV=compose_default" {
			defaultIdx = i
		}
	}
	if defaultIdx >= 0 && (bareIdx < 0 || bareIdx < defaultIdx) {
		t.Fatalf("CLI bare -e APP_ENV appears before compose default -e APP_ENV=compose_default: %v", args)
	}
}

func TestAuditBugInspectContainerStateJSONFallbackMissing(t *testing.T) {
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "inspect" {
				return `[{"State":{"Status":"running"}}]`, nil
			}
			if len(args) > 0 && args[0] == "ps" {
				return "id1\tdemo-web-1\tnginx:alpine\tUp 5s\t\t", nil
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	st, err := InspectContainerState(context.Background(), "demo-web-1")
	if err != nil {
		t.Fatalf("InspectContainerState failed: %v", err)
	}
	if st.Status != "running" {
		t.Fatalf("expected status 'running', got %q", st.Status)
	}
}

func TestAuditBugSplitImageRefDigestAndImagesSwallowsErrors(t *testing.T) {
	repo, tag := splitImageRef("nginx@sha256:abcdef123456")
	if repo != "nginx" || tag != "sha256:abcdef123456" {
		t.Fatalf("splitImageRef misparsed digest ref into repo=%q tag=%q", repo, tag)
	}
	if trimRegistryPrefix("nginx") != "library/nginx" || trimRegistryPrefix("docker.io/library/nginx") != "library/nginx" {
		t.Fatalf("expected official image normalization to preserve library/: got %q / %q", trimRegistryPrefix("nginx"), trimRegistryPrefix("docker.io/library/nginx"))
	}
	if trimRegistryPrefix("ghcr.io/library/app") != "ghcr.io/library/app" {
		t.Fatalf("expected private registry library/ path preserved, got %q", trimRegistryPrefix("ghcr.io/library/app"))
	}

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			return "", errors.New("daemon unavailable")
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
	if err := Images(context.Background(), cfg, nil); err == nil {
		t.Fatalf("Images silently swallowed ListImages error")
	}
}

func TestAuditBugCPRejectsSingleLetterServiceName(t *testing.T) {
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"a": {Image: "alpine:latest"},
		},
	}
	if err := CP(context.Background(), cfg, "a:/etc/hosts", `C:\tmp\hosts`); err != nil {
		t.Fatalf("CP rejected valid single-letter service 'a': %v", err)
	}
}

func TestAuditBugListVolumesDropsVolumeMatchingHeaderKeyword(t *testing.T) {
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) >= 3 && args[0] == "volume" && args[1] == "ls" && args[2] == "-q" {
				return "app_driver_data\nother_vol", nil
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	vols, err := ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes failed: %v", err)
	}
	want := map[string]bool{"app_driver_data": true, "other_vol": true}
	if !reflect.DeepEqual(vols, want) {
		t.Fatalf("ListVolumes dropped volume containing 'driver': got %v, want %v", vols, want)
	}
}

func TestAuditBugSelectRMIImagesLocalRemovesExplicitImageWithBuild(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"api": {
				Image: "myorg/api:1.0",
				Build: config.BuildConfig{Context: "."},
			},
			"worker": {
				Build: config.BuildConfig{Context: "./worker"},
			},
		},
	}

	got := selectRMIImages(cfg, "local")
	want := []string{"demo-worker:latest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectRMIImages local included explicitly named image: got %v, want %v", got, want)
	}
}

func TestAuditBugNoDefaultProjectNetworkCreatedOrAttached(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "myapp",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
			"api": {Image: "golang:alpine"},
		},
	}
	attachments := resolveNetworkAttachments(cfg, "web", cfg.Services["web"])
	if len(attachments) != 1 || attachments[0].Network != "myapp_default" || !reflect.DeepEqual(attachments[0].Aliases, []string{"web"}) {
		t.Fatalf("expected default project network attachment myapp_default with alias web, got %+v", attachments)
	}
}
