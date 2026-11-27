package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"wslc-compose/internal/config"
)

func TestBindMountPreValidationAndBaseDirResolution(t *testing.T) {
	t.Run("missing bind mount fails before calling wslc", func(t *testing.T) {
		baseDir := t.TempDir()
		called := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				called = true
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				called = true
				return "", nil
			},
		)

		cfg := config.ComposeFile{
			Name:    "demo",
			BaseDir: baseDir,
			Services: map[string]config.Service{
				"web": {
					Image: "nginx:alpine",
					Volumes: config.VolumeMounts{
						{
							Type:   "bind",
							Source: "./nginx/default.conf",
							Target: "/etc/nginx/conf.d/default.conf",
						},
					},
				},
			},
		}

		err := Up(context.Background(), cfg, UpOptions{Detach: true})
		if err == nil {
			t.Fatalf("expected bind mount validation error, got nil")
		}
		wantErr := fmt.Sprintf("bind mount source %q for service %q does not exist", filepath.Join(baseDir, "nginx", "default.conf"), "web")
		if err.Error() != wantErr {
			t.Fatalf("got error %q, want %q", err.Error(), wantErr)
		}
		if called {
			t.Fatalf("wslc must not be invoked when bind mount validation fails")
		}
	})

	t.Run("missing file-like target fails before calling wslc", func(t *testing.T) {
		baseDir := t.TempDir()
		called := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				called = true
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				called = true
				return "", nil
			},
		)

		cfg := config.ComposeFile{
			Name:    "demo",
			BaseDir: baseDir,
			Services: map[string]config.Service{
				"web": {
					Image: "nginx:alpine",
					Volumes: config.VolumeMounts{
						{
							Source: "default.conf",
							Target: "/etc/nginx/conf.d/default.conf",
						},
					},
				},
			},
		}

		err := Up(context.Background(), cfg, UpOptions{Detach: true})
		if err == nil {
			t.Fatalf("expected error for missing file-like bind mount")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Fatalf("wslc must not be invoked when file-like bind mount is missing")
		}
	})

	t.Run("existing bind mount and env_file resolve against BaseDir", func(t *testing.T) {
		baseDir := t.TempDir()
		confDir := filepath.Join(baseDir, "nginx")
		if err := os.MkdirAll(confDir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		confFile := filepath.Join(confDir, "default.conf")
		if err := os.WriteFile(confFile, []byte("server {}"), 0644); err != nil {
			t.Fatalf("failed to create conf file: %v", err)
		}

		var runArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "run" {
					runArgs = append([]string(nil), args...)
				}
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		cfg := config.ComposeFile{
			Name:    "demo",
			BaseDir: baseDir,
			Services: map[string]config.Service{
				"web": {
					Image:   "nginx:alpine",
					EnvFile: config.StringList{".env.web"},
					Volumes: config.VolumeMounts{
						{
							Type:     "bind",
							Source:   "./nginx/default.conf",
							Target:   "/etc/nginx/conf.d/default.conf",
							ReadOnly: true,
						},
					},
				},
			},
		}

		if err := Up(context.Background(), cfg, UpOptions{Detach: true}); err != nil {
			t.Fatalf("Up failed: %v", err)
		}

		joined := strings.Join(runArgs, " ")
		wantEnv := "--env-file " + filepath.Join(baseDir, ".env.web")
		wantMount := "-v " + confFile + ":/etc/nginx/conf.d/default.conf:ro"
		if !strings.Contains(joined, wantEnv) {
			t.Fatalf("expected run args to contain %q, got: %s", wantEnv, joined)
		}
		if !strings.Contains(joined, wantMount) {
			t.Fatalf("expected run args to contain %q, got: %s", wantMount, joined)
		}
	})

	t.Run("run validates missing bind mounts before wslc", func(t *testing.T) {
		baseDir := t.TempDir()
		called := false
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				called = true
				return "ok", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				called = true
				return "", nil
			},
		)

		cfg := config.ComposeFile{
			Name:    "demo",
			BaseDir: baseDir,
			Services: map[string]config.Service{
				"web": {
					Image: "nginx:alpine",
					Volumes: config.VolumeMounts{
						{Type: "bind", Source: "./missing.json", Target: "/app/config.json"},
					},
				},
			},
		}

		err := Run(context.Background(), cfg, "web", []string{"true"}, RunOptions{Detach: true})
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("expected missing bind mount error from Run, got: %v", err)
		}
		if called {
			t.Fatalf("wslc must not be invoked when Run bind mount validation fails")
		}
	})
}

func TestBuildImagePullAndBuildArgOverrides(t *testing.T) {
	var gotArgs []string
	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			gotArgs = append([]string(nil), args...)
			return "", nil
		},
	)

	buildCfg := config.BuildConfig{
		Context:    "./app",
		Dockerfile: "Dockerfile.prod",
		Target:     "release",
		Args: map[string]string{
			"GO_VERSION": "1.22",
			"ENV":        "dev",
		},
	}

	err := BuildImage(context.Background(), "demo-api:latest", buildCfg, BuildOptions{
		NoCache:    true,
		Pull:       true,
		BuildArgs:  []string{"ENV=prod", "COMMIT=abc123"},
		ProjectDir: "/workspace",
	})
	if err != nil {
		t.Fatalf("BuildImage failed: %v", err)
	}

	wantCtx := filepath.Join("/workspace", "app")
	wantDockerfile := filepath.Join(wantCtx, "Dockerfile.prod")
	wantArgs := []string{
		"build", "-t", "demo-api:latest",
		"--no-cache",
		"--pull",
		"-f", wantDockerfile,
		"--target", "release",
		"--build-arg", "COMMIT=abc123",
		"--build-arg", "ENV=prod",
		"--build-arg", "GO_VERSION=1.22",
		wantCtx,
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("got build args %v, want %v", gotArgs, wantArgs)
	}
}

func TestCPTranslationAndOptions(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}

	t.Run("container to windows host with archive", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		err := CP(context.Background(), cfg, "web:/etc/nginx/nginx.conf", `C:\tmp\nginx.conf`, CPOptions{Archive: true})
		if err != nil {
			t.Fatalf("CP failed: %v", err)
		}
		want := []string{"cp", "-a", "demo-web-1:/etc/nginx/nginx.conf", `C:\tmp\nginx.conf`}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Fatalf("got cp args %v, want %v", gotArgs, want)
		}
	})

	t.Run("windows host to container with follow-link", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		err := CP(context.Background(), cfg, `C:\local\app.conf`, "web:/app/app.conf", CPOptions{FollowLink: true})
		if err != nil {
			t.Fatalf("CP failed: %v", err)
		}
		want := []string{"cp", "-L", `C:\local\app.conf`, "demo-web-1:/app/app.conf"}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Fatalf("got cp args %v, want %v", gotArgs, want)
		}
	})

	t.Run("rejects invalid endpoints", func(t *testing.T) {
		if err := CP(context.Background(), cfg, `C:\a.txt`, `C:\b.txt`); err == nil {
			t.Fatalf("expected error when neither side specifies a service")
		}
		if err := CP(context.Background(), cfg, "web:/a", "web:/b"); err == nil {
			t.Fatalf("expected error when both sides specify services")
		}
		if err := CP(context.Background(), cfg, "missing:/a", `C:\b.txt`); err == nil {
			t.Fatalf("expected error for unknown service")
		}
	})
}

func TestPortLookupInspectAndFallback(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}

	t.Run("parses inspect json for tcp and udp", func(t *testing.T) {
		inspectJSON := `[{"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}],"53/udp":[{"HostIp":"","HostPort":"5353"}]}}}]`
		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "inspect" && args[1] == "demo-web-1" {
					return inspectJSON, nil
				}
				return "", ErrNotFound
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := Port(context.Background(), cfg, "web", "80"); err != nil {
			t.Fatalf("Port tcp failed: %v", err)
		}
		if err := Port(context.Background(), cfg, "web", "53", PortOptions{Protocol: "udp"}); err != nil {
			t.Fatalf("Port udp failed: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
		want := []string{"127.0.0.1:8080", "0.0.0.0:5353"}
		if !reflect.DeepEqual(lines, want) {
			t.Fatalf("got port lines %v, want %v", lines, want)
		}
	})

	t.Run("falls back to container list ports column", func(t *testing.T) {
		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					return "{}", nil
				}
				if len(args) > 0 && args[0] == "ps" {
					return "abc123\tdemo-web-1\tnginx:alpine\tUp 5s\t127.0.0.1:9090->80/tcp\t", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := Port(context.Background(), cfg, "web", "80"); err != nil {
			t.Fatalf("Port fallback failed: %v", err)
		}
		if strings.TrimSpace(outBuf.String()) != "127.0.0.1:9090" {
			t.Fatalf("expected 127.0.0.1:9090, got %q", outBuf.String())
		}
	})

	t.Run("validates protocol and private port", func(t *testing.T) {
		if err := Port(context.Background(), cfg, "web", "80", PortOptions{Protocol: "icmp"}); err == nil {
			t.Fatalf("expected error for invalid protocol")
		}
		if err := Port(context.Background(), cfg, "web", "notaport"); err == nil {
			t.Fatalf("expected error for invalid port")
		}
	})
}

func TestKillCommandSignalAndDefault(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
		},
	}

	var (
		mu    sync.Mutex
		calls [][]string
	)

	withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			mu.Lock()
			calls = append(calls, append([]string(nil), args...))
			mu.Unlock()
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			return "", nil
		},
	)

	if err := Kill(context.Background(), cfg, []string{"web"}); err != nil {
		t.Fatalf("Kill default failed: %v", err)
	}
	if err := Kill(context.Background(), cfg, []string{"web"}, KillOptions{Signal: "SIGTERM"}); err != nil {
		t.Fatalf("Kill SIGTERM failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := [][]string{
		{"kill", "-s", "SIGKILL", "demo-web-1"},
		{"kill", "-s", "SIGTERM", "demo-web-1"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("got kill calls %v, want %v", calls, want)
	}
}

func TestStatsCommandNoStreamAndLive(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"web": {Image: "nginx:alpine"},
			"db":  {Image: "postgres:16-alpine"},
		},
	}

	t.Run("no-stream prints single snapshot for running containers only", func(t *testing.T) {
		var statsArgs []string
		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "ps" {
					return "id1\tdemo-web-1\tnginx:alpine\tUp 10s\t\t\nid2\tdemo-db-1\tpostgres:16-alpine\tExited (0) 2s ago\t\t", nil
				}
				if len(args) > 0 && args[0] == "stats" {
					statsArgs = append([]string(nil), args...)
					return "NAME        CPU %   MEM USAGE\ndemo-web-1  1.5%    12MiB / 4GiB", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := Stats(context.Background(), cfg, nil, StatsOptions{NoStream: true}); err != nil {
			t.Fatalf("Stats --no-stream failed: %v", err)
		}

		wantArgs := []string{"stats", "demo-web-1"}
		if !reflect.DeepEqual(statsArgs, wantArgs) {
			t.Fatalf("got stats args %v, want %v", statsArgs, wantArgs)
		}
		if !strings.Contains(outBuf.String(), "demo-web-1") {
			t.Fatalf("expected stats output to contain demo-web-1, got:\n%s", outBuf.String())
		}
		if strings.Contains(outBuf.String(), "\033[H\033[2J") {
			t.Fatalf("did not expect ANSI screen clear in --no-stream mode, got:\n%q", outBuf.String())
		}
	})

	t.Run("live mode clears screen and polls until canceled", func(t *testing.T) {
		origInterval := statsPollInterval
		origTTY := isStdoutTTYFunc
		statsPollInterval = 10 * time.Millisecond
		isStdoutTTYFunc = func() bool { return true }
		t.Cleanup(func() {
			statsPollInterval = origInterval
			isStdoutTTYFunc = origTTY
		})

		var (
			mu    sync.Mutex
			polls int
		)
		secondPoll := make(chan struct{})

		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "ps" {
					return "id1\tdemo-web-1\tnginx:alpine\tUp 10s\t\t", nil
				}
				if len(args) > 0 && args[0] == "stats" {
					mu.Lock()
					polls++
					p := polls
					mu.Unlock()
					if p == 2 {
						close(secondPoll)
					}
					return "demo-web-1 1.0%", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Stats(ctx, cfg, []string{"web"}, StatsOptions{NoStream: false})
		}()

		select {
		case <-secondPoll:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for live stats polling")
		}
		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Stats live returned unexpected error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for Stats live to exit")
		}

		if strings.Count(outBuf.String(), "\033[H\033[2J") < 2 {
			t.Fatalf("expected live stats to emit in-place ANSI clear before each snapshot, got:\n%q", outBuf.String())
		}
	})

	t.Run("live mode omits ANSI clear when stdout is not a TTY", func(t *testing.T) {
		origInterval := statsPollInterval
		origTTY := isStdoutTTYFunc
		statsPollInterval = 10 * time.Millisecond
		isStdoutTTYFunc = func() bool { return false }
		t.Cleanup(func() {
			statsPollInterval = origInterval
			isStdoutTTYFunc = origTTY
		})

		var (
			mu    sync.Mutex
			polls int
		)
		secondPoll := make(chan struct{})

		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "ps" {
					return "id1\tdemo-web-1\tnginx:alpine\tUp 10s\t\t", nil
				}
				if len(args) > 0 && args[0] == "stats" {
					mu.Lock()
					polls++
					p := polls
					mu.Unlock()
					if p == 2 {
						close(secondPoll)
					}
					return "demo-web-1 1.0%", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Stats(ctx, cfg, []string{"web"}, StatsOptions{NoStream: false})
		}()

		select {
		case <-secondPoll:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for non-TTY live stats polling")
		}
		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Stats non-TTY live returned unexpected error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for Stats non-TTY live to exit")
		}

		if strings.Contains(outBuf.String(), "\033[H\033[2J") {
			t.Fatalf("did not expect ANSI clear codes when stdout is not a TTY, got:\n%q", outBuf.String())
		}
		if strings.Count(outBuf.String(), "demo-web-1 1.0%") < 2 {
			t.Fatalf("expected stats snapshots to still be written without TTY, got:\n%q", outBuf.String())
		}
	})
}

func TestEventsCommandFiltersProjectContainersAndJSON(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"api": {Image: "golang:alpine"},
			"db":  {Image: "postgres:16-alpine"},
		},
	}

	t.Run("filters incoming events in Go by project container name and id", func(t *testing.T) {
		var gotArgs []string
		doneEmitting := make(chan struct{})

		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "ps" {
					return "90aecf63bb09abcdef\tdemo-db-1\tpostgres:16-alpine\tUp 5s\t\t", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				onLine("TIMESTAMP             TYPE       ACTION   CONTAINER")
				onLine("2026-11-26T20:00:00Z  container  start    other-app-1")
				onLine("2026-11-26T20:00:01Z  container  start    demo-api-1")
				onLine("2026-11-26T20:00:02Z  container  stop     90aecf63bb09")
				onLine("2026-11-26T20:00:03Z  container  start    demo-api-10")
				close(doneEmitting)
				<-ctx.Done()
				return "", ctx.Err()
			},
		)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Events(ctx, cfg, nil)
		}()

		select {
		case <-doneEmitting:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for events lines")
		}
		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Events returned unexpected error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for Events to exit")
		}

		wantArgs := []string{"events"}
		if !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("got events args %v, want %v", gotArgs, wantArgs)
		}

		output := outBuf.String()
		if !strings.Contains(output, "demo-api-1") || !strings.Contains(output, "90aecf63bb09") {
			t.Fatalf("expected matching project container events in output, got:\n%s", output)
		}
		if strings.Contains(output, "other-app-1") || strings.Contains(output, "demo-api-10") || strings.Contains(output, "TIMESTAMP") {
			t.Fatalf("expected non-project events and headers to be filtered out, got:\n%s", output)
		}
	})

	t.Run("formats matched events as json objects when --json is set", func(t *testing.T) {
		var gotArgs []string
		doneEmitting := make(chan struct{})

		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				onLine("2026-11-26T20:00:00Z container start unrelated-1")
				onLine("2026-11-26T20:00:01Z container start demo-api-1")
				close(doneEmitting)
				<-ctx.Done()
				return "", ctx.Err()
			},
		)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Events(ctx, cfg, []string{"api"}, EventsOptions{JSON: true})
		}()

		select {
		case <-doneEmitting:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for json event")
		}
		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Events --json returned unexpected error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for Events --json to exit")
		}

		if !reflect.DeepEqual(gotArgs, []string{"events"}) {
			t.Fatalf("expected plain 'wslc events' invocation, got %v", gotArgs)
		}

		rawLine := strings.TrimSpace(outBuf.String())
		var decoded composeEventJSON
		if err := json.Unmarshal([]byte(rawLine), &decoded); err != nil {
			t.Fatalf("expected valid json output, got %q (err: %v)", rawLine, err)
		}
		if decoded.Timestamp != "2026-11-26T20:00:01Z" || decoded.Container != "demo-api-1" || decoded.Service != "api" || decoded.Event != "container start demo-api-1" {
			t.Fatalf("unexpected decoded json event: %+v", decoded)
		}
	})

	t.Run("returns non-zero wslc events error when not canceled", func(t *testing.T) {
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", errors.New("events stream failed")
			},
		)

		err := Events(context.Background(), cfg, nil)
		if err == nil || !strings.Contains(err.Error(), "events stream failed") {
			t.Fatalf("expected error from failed wslc events, got: %v", err)
		}
	})
}

func TestBindMountValidationSkipsPOSIXPathsOnWindowsAndDownTimeoutBehavior(t *testing.T) {
	t.Run("POSIX bind mount passes validation", func(t *testing.T) {
		cfg := config.ComposeFile{
			Name:    "demo",
			BaseDir: t.TempDir(),
			Services: map[string]config.Service{
				"agent": {
					Image: "alpine:latest",
					Volumes: config.VolumeMounts{
						{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
					},
				},
			},
		}
		if os.PathSeparator == '\\' {
			if err := validateServiceBindMounts(cfg, "agent", cfg.Services["agent"]); err != nil {
				t.Fatalf("POSIX path bind mount should skip Windows host os.Stat: %v", err)
			}
		}
	})

	t.Run("Down defaults to 10s stop timeout and skips stop on explicit timeout 0", func(t *testing.T) {
		var (
			mu    sync.Mutex
			calls [][]string
		)
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				mu.Lock()
				calls = append(calls, append([]string(nil), args...))
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

		if err := Down(context.Background(), cfg, DownOptions{RemoveVolumes: true}); err != nil {
			t.Fatalf("Down failed: %v", err)
		}
		if len(calls) < 2 || !reflect.DeepEqual(calls[0], []string{"stop", "-t", "10", "demo-db-1"}) {
			t.Fatalf("expected default 10s graceful stop, got calls: %v", calls)
		}

		calls = nil
		if err := Down(context.Background(), cfg, DownOptions{Timeout: 0, HasTimeout: true}); err != nil {
			t.Fatalf("Down with explicit timeout 0 failed: %v", err)
		}
		if len(calls) == 0 || calls[0][0] != "remove" {
			t.Fatalf("expected explicit --timeout 0 to skip stop and remove immediately, got calls: %v", calls)
		}
	})

	t.Run("Port auto-resolves UDP-only binding when protocol omitted", func(t *testing.T) {
		inspectJSON := `[{"NetworkSettings":{"Ports":{"53/udp":[{"HostIp":"0.0.0.0","HostPort":"5353"}]}}}]`
		outBuf := withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "inspect" {
					return inspectJSON, nil
				}
				return "", ErrNotFound
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)
		cfg := config.ComposeFile{
			Name: "demo",
			Services: map[string]config.Service{
				"dns": {Image: "coredns/coredns"},
			},
		}
		if err := Port(context.Background(), cfg, "dns", "53"); err != nil {
			t.Fatalf("Port failed for UDP-only binding: %v", err)
		}
		if strings.TrimSpace(outBuf.String()) != "0.0.0.0:5353" {
			t.Fatalf("got %q, want 0.0.0.0:5353", strings.TrimSpace(outBuf.String()))
		}
	})
}

func TestCPSingleLetterServiceAndWindowsDrivePaths(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"a": {Image: "alpine:latest"},
		},
	}

	if !isWindowsDrivePath(`C:\temp\file.txt`) || !isWindowsDrivePath(`C:/temp/file.txt`) || !isWindowsDrivePath(`C:`) {
		t.Fatalf("expected standard Windows drive paths to be detected")
	}
	if isWindowsDrivePath("a:file.txt") {
		t.Fatalf("a:file.txt must not be treated as a Windows drive path")
	}

	t.Run("a:/etc/hosts resolves to container while C:\\temp\\file.txt stays host path", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := CP(context.Background(), cfg, "a:/etc/hosts", `C:\temp\file.txt`); err != nil {
			t.Fatalf("CP failed for a:/etc/hosts: %v", err)
		}
		want := []string{"cp", "demo-a-1:/etc/hosts", `C:\temp\file.txt`}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Fatalf("got cp args %v, want %v", gotArgs, want)
		}
	})

	t.Run("a:file.txt resolves to container path", func(t *testing.T) {
		var gotArgs []string
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				gotArgs = append([]string(nil), args...)
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := CP(context.Background(), cfg, "a:file.txt", `C:\temp\file.txt`); err != nil {
			t.Fatalf("CP failed for a:file.txt: %v", err)
		}
		want := []string{"cp", "demo-a-1:file.txt", `C:\temp\file.txt`}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Fatalf("got cp args %v, want %v", gotArgs, want)
		}
	})

	if extracted := ExtractCPServices("a:/etc/hosts", `C:\temp\file.txt`); !reflect.DeepEqual(extracted, []string{"a"}) {
		t.Fatalf("ExtractCPServices returned %v, want [a]", extracted)
	}
}

func TestEventsDynamicIDRefreshOnContainerRecreate(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"db": {Image: "postgres:16-alpine"},
		},
	}

	var (
		mu        sync.Mutex
		recreated bool
	)
	doneEmitting := make(chan struct{})

	outBuf := withMockRuntime(
		t,
		func(ctx context.Context, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "ps" {
				mu.Lock()
				isRecreated := recreated
				mu.Unlock()
				if isRecreated {
					return "222222222222bbbb\tdemo-db-1\tpostgres:16-alpine\tUp 1s\t\t", nil
				}
				return "111111111111aaaa\tdemo-db-1\tpostgres:16-alpine\tUp 5s\t\t", nil
			}
			return "", nil
		},
		func(ctx context.Context, onLine func(string), args ...string) (string, error) {
			onLine("2026-11-26T20:00:01Z container die 111111111111")
			mu.Lock()
			recreated = true
			mu.Unlock()
			onLine("2026-11-26T20:00:02Z container start 222222222222")
			close(doneEmitting)
			<-ctx.Done()
			return "", ctx.Err()
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Events(ctx, cfg, nil)
	}()

	select {
	case <-doneEmitting:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for recreated container event")
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Events returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for Events to exit")
	}

	output := outBuf.String()
	if !strings.Contains(output, "111111111111") || !strings.Contains(output, "222222222222") {
		t.Fatalf("expected events for both original and recreated container IDs, got:\n%s", output)
	}
}
