package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"wslc-compose/internal/config"
)

type LogsOptions struct {
	Follow     bool
	Tail       string
	Timestamps bool
}

func Logs(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...LogsOptions) error {
	var opt LogsOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	maxNameLen := 0
	for _, name := range names {
		if len(name) > maxNameLen {
			maxNameLen = len(name)
		}
	}

	out := stdout()
	tty := isStdoutTTY()
	var (
		printMu sync.Mutex
		errMu   sync.Mutex
		errs    []error
		wg      sync.WaitGroup
	)

	for idx, svcName := range names {
		name := svcName
		containerName := cfg.ContainerName(name)
		color := logColors[idx%len(logColors)]
		padded := fmt.Sprintf("%-*s", maxNameLen, name)
		prefix := padded + " | "
		if tty {
			prefix = color + padded + ansiReset + " " + ansiDim + "|" + ansiReset + " "
		}

		args := []string{"logs"}
		if opt.Follow {
			args = append(args, "-f")
		}
		if opt.Timestamps {
			args = append(args, "-t")
		}
		if opt.Tail != "" && !strings.EqualFold(opt.Tail, "all") {
			args = append(args, "--tail", opt.Tail)
		}
		args = append(args, containerName)

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, runErr := RunWSLCStream(ctx, func(line string) {
				printMu.Lock()
				fmt.Fprintf(out, "%s%s\n", prefix, line)
				printMu.Unlock()
			}, args...)
			if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, ErrNotFound) {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("failed to read logs for service %q: %w", name, runErr))
				errMu.Unlock()
			}
		}()
	}

	wg.Wait()
	return errors.Join(errs...)
}
