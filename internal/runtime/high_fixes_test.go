package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"wslc-compose/internal/config"
)

func TestHelperWSLCStreamProcess(t *testing.T) {
	if os.Getenv("GO_WANT_WSLC_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("GO_WSLC_HELPER_MODE")
	switch mode {
	case "large-and-cr":
		_, _ = os.Stdout.WriteString("step1\rprogress 50%\r\n")
		_, _ = os.Stdout.WriteString(strings.Repeat("x", maxStreamLineBytes+4096))
		_, _ = os.Stdout.WriteString("\nfinal-stdout\n")
		_, _ = os.Stderr.WriteString(strings.Repeat("e", maxStreamLineBytes+2048))
		_, _ = os.Stderr.WriteString("\n")
		os.Exit(0)
	case "logs-not-found":
		_, _ = os.Stderr.WriteString("Error response from daemon:\nNo such container: demo-missing-1\n")
		os.Exit(1)
	case "logs-stderr-app":
		_, _ = os.Stderr.WriteString("2026-11-27 LOG: database system is ready\nWarning: cache item not found\n")
		os.Exit(0)
	case "logs-daemon-error":
		_, _ = os.Stderr.WriteString("Error: daemon socket connection refused\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func withHelperWSLCCommand(t *testing.T, mode string) *lockedBuffer {
	t.Helper()
	origCmd := wslcCommandFunc
	origStream := execWSLCStreamFunc
	origOut := stdoutWriter

	buf := &lockedBuffer{}
	stdoutWriter = buf
	execWSLCStreamFunc = runWSLCStream
	wslcCommandFunc = func(ctx context.Context, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=^TestHelperWSLCStreamProcess$", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_WSLC_HELPER_PROCESS=1",
			"GO_WSLC_HELPER_MODE="+mode,
		)
		return cmd
	}

	t.Cleanup(func() {
		wslcCommandFunc = origCmd
		execWSLCStreamFunc = origStream
		stdoutWriter = origOut
	})
	return buf
}

func TestHighFix2RunWSLCStreamDrainsLargeLinesAndCRWithoutDeadlock(t *testing.T) {
	withHelperWSLCCommand(t, "large-and-cr")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	out, err := RunWSLCStream(ctx, func(line string) {
		lines = append(lines, line)
	}, "pull", "demo:latest")
	if err != nil {
		t.Fatalf("RunWSLCStream failed: %v", err)
	}

	foundStep1 := false
	foundProgress := false
	foundFinal := false
	foundMaxChunk := false
	for _, l := range lines {
		if l == "step1" {
			foundStep1 = true
		}
		if l == "progress 50%" {
			foundProgress = true
		}
		if l == "final-stdout" {
			foundFinal = true
		}
		if len(l) == maxStreamLineBytes {
			foundMaxChunk = true
		}
	}

	if !foundStep1 || !foundProgress || !foundFinal || !foundMaxChunk {
		t.Fatalf("missing expected stream segments: step1=%v progress=%v final=%v maxChunk=%v", foundStep1, foundProgress, foundFinal, foundMaxChunk)
	}
	if !strings.HasSuffix(out, "final-stdout") {
		t.Fatalf("expected returned stdout summary to end with 'final-stdout', got suffix %q", out[max(0, len(out)-32):])
	}
}

func TestHighFix2LogsSuppressesStderrOnErrNotFoundAndKeepsContainerStderr(t *testing.T) {
	cfg := config.ComposeFile{
		Name: "demo",
		Services: map[string]config.Service{
			"missing": {Image: "alpine:latest"},
		},
	}

	t.Run("suppresses wslc stderr when container is missing in non-follow and follow modes", func(t *testing.T) {
		for _, follow := range []bool{false, true} {
			outBuf := withHelperWSLCCommand(t, "logs-not-found")
			if err := Logs(context.Background(), cfg, []string{"missing"}, LogsOptions{Follow: follow}); err != nil {
				t.Fatalf("Logs(Follow=%v) returned error on ErrNotFound: %v", follow, err)
			}
			if outBuf.String() != "" {
				t.Fatalf("Logs(Follow=%v) leaked stderr to stdout on ErrNotFound: %q", follow, outBuf.String())
			}
		}
	})

	t.Run("preserves real container stderr logs when command succeeds", func(t *testing.T) {
		dbCfg := config.ComposeFile{
			Name: "demo",
			Services: map[string]config.Service{
				"db": {Image: "postgres:16-alpine"},
			},
		}
		for _, follow := range []bool{false, true} {
			outBuf := withHelperWSLCCommand(t, "logs-stderr-app")
			if err := Logs(context.Background(), dbCfg, []string{"db"}, LogsOptions{Follow: follow}); err != nil {
				t.Fatalf("Logs(Follow=%v) failed: %v", follow, err)
			}
			got := outBuf.String()
			if !strings.Contains(got, "db | 2026-11-27 LOG: database system is ready") || !strings.Contains(got, "db | Warning: cache item not found") {
				t.Fatalf("Logs(Follow=%v) dropped valid container stderr output: %q", follow, got)
			}
		}
	})

	t.Run("returns non-notfound error without printing wslc error to stdout", func(t *testing.T) {
		outBuf := withHelperWSLCCommand(t, "logs-daemon-error")
		err := Logs(context.Background(), cfg, []string{"missing"}, LogsOptions{Follow: false})
		if err == nil || !strings.Contains(err.Error(), "daemon socket connection refused") {
			t.Fatalf("expected daemon error from Logs, got: %v", err)
		}
		if outBuf.String() != "" {
			t.Fatalf("did not expect daemon error to be printed as container stdout log, got: %q", outBuf.String())
		}
	})
}

func TestHighFix3WaitForHealthyHandlesPausedRemovingRestartingAndCreated(t *testing.T) {
	hcSvc := config.Service{
		Image: "postgres:16-alpine",
		HealthCheck: &config.HealthCheck{
			Test:     config.HealthCheckTest{"CMD", "pg_isready"},
			Interval: "5ms",
			Retries:  3,
		},
	}

	for _, immediateState := range []string{"paused", "removing"} {
		t.Run("aborts immediately on "+immediateState, func(t *testing.T) {
			polls := 0
			withMockRuntime(
				t,
				func(ctx context.Context, args ...string) (string, error) {
					if len(args) > 0 && args[0] == "inspect" {
						polls++
						return fmt.Sprintf("%s|starting|0", immediateState), nil
					}
					return "", nil
				},
				func(ctx context.Context, onLine func(string), args ...string) (string, error) {
					return "", nil
				},
			)

			err := WaitForCondition(context.Background(), "demo-db-1", hcSvc, "service_healthy")
			if err == nil || !strings.Contains(err.Error(), immediateState) {
				t.Fatalf("expected descriptive error for %s, got: %v", immediateState, err)
			}
			if polls != 1 {
				t.Fatalf("expected immediate abort in 1 poll for %s, got %d", immediateState, polls)
			}
		})
	}

	for _, stuck := range []struct {
		name        string
		inspectLine string
		svc         config.Service
	}{
		{"restarting_with_starting_health", "restarting|starting|1", hcSvc},
		{"restarting_with_empty_health", "restarting||1", hcSvc},
		{"created_with_healthcheck", "created|starting|0", hcSvc},
		{"created_without_healthcheck", "created||0", config.Service{
			Image: "alpine:latest",
			HealthCheck: &config.HealthCheck{
				Disable:  true,
				Interval: "5ms",
				Retries:  3,
			},
		}},
	} {
		t.Run("respects retries and aborts on "+stuck.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				polls int
			)
			withMockRuntime(
				t,
				func(ctx context.Context, args ...string) (string, error) {
					if len(args) > 0 && args[0] == "inspect" {
						mu.Lock()
						polls++
						mu.Unlock()
						return stuck.inspectLine, nil
					}
					return "", nil
				},
				func(ctx context.Context, onLine func(string), args ...string) (string, error) {
					return "", nil
				},
			)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			err := WaitForCondition(ctx, "demo-db-1", stuck.svc, "service_healthy")
			if err == nil || ctx.Err() != nil {
				t.Fatalf("expected retry exhaustion error before context timeout, got err=%v ctxErr=%v", err, ctx.Err())
			}
			mu.Lock()
			gotPolls := polls
			mu.Unlock()
			if gotPolls != 3 {
				t.Fatalf("expected 3 polls matching Retries=3, got %d", gotPolls)
			}
		})
	}

	t.Run("transient created state recovers to healthy before retries exhausted", func(t *testing.T) {
		polls := 0
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					polls++
					if polls < 3 {
						return "created|starting|0", nil
					}
					return "running|healthy|0", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := WaitForCondition(context.Background(), "demo-db-1", hcSvc, "service_healthy"); err != nil {
			t.Fatalf("expected recovery from created to healthy, got: %v", err)
		}
	})
}

func TestHighFix4WaitForCompletedHandlesRestartingPausedRemovingAndCreated(t *testing.T) {
	origInterval := waitCompletedPollInterval
	waitCompletedPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		waitCompletedPollInterval = origInterval
	})

	svc := config.Service{Image: "alpine:latest"}

	for _, badState := range []string{"restarting", "paused", "removing"} {
		t.Run("aborts immediately on "+badState, func(t *testing.T) {
			polls := 0
			withMockRuntime(
				t,
				func(ctx context.Context, args ...string) (string, error) {
					if len(args) > 0 && args[0] == "inspect" {
						polls++
						return badState + "||1", nil
					}
					return "", nil
				},
				func(ctx context.Context, onLine func(string), args ...string) (string, error) {
					return "", nil
				},
			)

			err := WaitForCondition(context.Background(), "demo-init-1", svc, "service_completed_successfully")
			if err == nil || !strings.Contains(err.Error(), badState) {
				t.Fatalf("expected immediate error mentioning %q, got: %v", badState, err)
			}
			if polls != 1 {
				t.Fatalf("expected 1 poll on %s, got %d", badState, polls)
			}
		})
	}

	t.Run("aborts when stuck permanently in created state", func(t *testing.T) {
		polls := 0
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					polls++
					return "created||0", nil
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := WaitForCondition(ctx, "demo-init-1", svc, "service_completed_successfully")
		if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "created") {
			t.Fatalf("expected stuck-created error before timeout, got err=%v ctxErr=%v", err, ctx.Err())
		}
		if polls != maxCreatedWaitPolls {
			t.Fatalf("expected %d polls, got %d", maxCreatedWaitPolls, polls)
		}
	})

	t.Run("transient created then running then exited 0 succeeds", func(t *testing.T) {
		polls := 0
		withMockRuntime(
			t,
			func(ctx context.Context, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "inspect" {
					polls++
					switch polls {
					case 1, 2:
						return "created||0", nil
					case 3:
						return "running||0", nil
					default:
						return "exited||0", nil
					}
				}
				return "", nil
			},
			func(ctx context.Context, onLine func(string), args ...string) (string, error) {
				return "", nil
			},
		)

		if err := WaitForCondition(context.Background(), "demo-init-1", svc, "service_completed_successfully"); err != nil {
			t.Fatalf("expected transient created container to complete cleanly, got: %v", err)
		}
	})
}
