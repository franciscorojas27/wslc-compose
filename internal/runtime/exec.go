package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
)

const (
	maxStreamLineBytes     = 1024 * 1024
	maxBufferedStreamLines = 256
)

type stderrCallbackKey struct{}

type streamPreserveSGRKey struct{}

func withStderrCallback(ctx context.Context, fn func(string)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, stderrCallbackKey{}, fn)
}

var (
	ErrAlreadyExists        = errors.New("resource already exists")
	ErrNotFound             = errors.New("resource not found")
	ansiEscapeRe            = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	ansiNonSGRRe            = regexp.MustCompile(`\x1b\[[0-9;?]*[a-ln-zA-Z]|\x1b\[\?[0-9;]*m`)
	wslcCommandFunc         func(ctx context.Context, args ...string) *exec.Cmd
	execWSLCFunc            = runWSLC
	execWSLCInteractiveFunc = runWSLCInteractive
	execWSLCStreamFunc      = runWSLCStream
)

func buildWSLCCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if wslcCommandFunc != nil {
		return wslcCommandFunc(ctx, args...), nil
	}
	bin, err := ResolveWSLCBinary()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return exec.CommandContext(ctx, bin, args...), nil
}

func ResolveWSLCBinary() (string, error) {
	candidates := []string{"wslc.exe", "wslc"}
	if goruntime.GOOS != "windows" {
		candidates = []string{"wslc", "wslc.exe"}
	}
	var firstErr error
	for _, name := range candidates {
		p, err := exec.LookPath(name)
		if err == nil && p != "" {
			return p, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", fmt.Errorf("'wslc' was not found in system PATH: %w", firstErr)
}

func withPreserveSGR(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, streamPreserveSGRKey{}, true)
}

func shouldPreserveSGR(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(streamPreserveSGRKey{}).(bool)
	return v
}

func stripNonSGRANSI(s string) string {
	return ansiNonSGRRe.ReplaceAllString(s, "")
}

func RunWSLC(ctx context.Context, args ...string) (string, error) {
	return execWSLCFunc(ctx, args...)
}

func runWSLC(ctx context.Context, args ...string) (string, error) {
	cmd, err := buildWSLCCommand(ctx, args...)
	if err != nil {
		return "", err
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err = cmd.Run()
	stdout := strings.TrimSpace(stdoutBuf.String())
	stderr := strings.TrimSpace(stderrBuf.String())

	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		rawErr := stderr
		if rawErr == "" {
			rawErr = stdout
		}
		return "", classifyWSLCError(rawErr, err)
	}

	return stdout, nil
}

func RunWSLCInteractive(ctx context.Context, args ...string) error {
	return execWSLCInteractiveFunc(ctx, args...)
}

func runWSLCInteractive(ctx context.Context, args ...string) error {
	cmd, err := buildWSLCCommand(ctx, args...)
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func RunWSLCStream(ctx context.Context, onLine func(string), args ...string) (string, error) {
	return execWSLCStreamFunc(ctx, onLine, args...)
}

func RunWSLCStreamRaw(ctx context.Context, onLine func(string), args ...string) (string, error) {
	return execWSLCStreamFunc(withPreserveSGR(ctx), onLine, args...)
}

func runWSLCStream(ctx context.Context, onLine func(string), args ...string) (string, error) {
	cmd, err := buildWSLCCommand(ctx, args...)
	if err != nil {
		return "", err
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	stderrCb, hasStderrCb := ctx.Value(stderrCallbackKey{}).(func(string))
	preserveSGR := shouldPreserveSGR(ctx)

	var (
		mu          sync.Mutex
		stdoutLines []string
		stderrLines []string
		wg          sync.WaitGroup
	)

	consume := func(r io.Reader, isStderr bool) {
		defer wg.Done()
		readStreamLines(r, func(rawLine string) {
			plain := stripANSI(rawLine)
			line := plain
			if preserveSGR {
				line = stripNonSGRANSI(rawLine)
			}
			mu.Lock()
			if isStderr {
				if strings.TrimSpace(plain) != "" {
					stderrLines = appendBoundedLine(stderrLines, plain, maxBufferedStreamLines)
				}
				if hasStderrCb {
					if stderrCb != nil {
						stderrCb(line)
					}
				} else if onLine != nil {
					onLine(line)
				}
			} else {
				stdoutLines = appendBoundedLine(stdoutLines, line, maxBufferedStreamLines)
				if onLine != nil {
					onLine(line)
				}
			}
			mu.Unlock()
		})
	}

	wg.Add(2)
	go consume(stdoutPipe, false)
	go consume(stderrPipe, true)

	wg.Wait()
	waitErr := cmd.Wait()

	if waitErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		cleanStdout := make([]string, len(stdoutLines))
		for i, l := range stdoutLines {
			cleanStdout[i] = stripANSI(l)
		}
		return "", classifyWSLCError(selectErrorRaw(stderrLines, cleanStdout), waitErr)
	}

	return strings.TrimSpace(strings.Join(stdoutLines, "\n")), nil
}

func scanStreamLines(r io.Reader, handle func(string)) {
	readStreamLines(r, handle)
}

func readStreamLines(r io.Reader, handle func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	chunk := make([]byte, 32*1024)
	var lineBuf []byte
	skipLeadingLF := false
	justFlushedMax := false

	emit := func() {
		handle(string(lineBuf))
		lineBuf = lineBuf[:0]
	}

	for {
		n, err := br.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			if skipLeadingLF && data[0] == '\n' {
				data = data[1:]
			}
			skipLeadingLF = false

			start := 0
			for i := 0; i < len(data); i++ {
				b := data[i]
				if b == '\n' || b == '\r' {
					if i > start {
						lineBuf = append(lineBuf, data[start:i]...)
						justFlushedMax = false
					}
					if len(lineBuf) > 0 || !justFlushedMax {
						emit()
					}
					justFlushedMax = false
					if b == '\r' {
						if i+1 < len(data) {
							if data[i+1] == '\n' {
								i++
							}
						} else {
							skipLeadingLF = true
						}
					}
					start = i + 1
				} else if len(lineBuf)+(i-start) >= maxStreamLineBytes {
					lineBuf = append(lineBuf, data[start:i]...)
					emit()
					justFlushedMax = true
					start = i
				}
			}

			if start < len(data) {
				lineBuf = append(lineBuf, data[start:]...)
				justFlushedMax = false
				if len(lineBuf) >= maxStreamLineBytes {
					emit()
					justFlushedMax = true
				}
			}
		}

		if err != nil {
			if len(lineBuf) > 0 {
				emit()
			}
			return
		}
	}
}

func appendBoundedLine(lines []string, line string, limit int) []string {
	if limit > 0 && len(lines) >= limit {
		copy(lines, lines[1:])
		lines[len(lines)-1] = line
		return lines
	}
	return append(lines, line)
}

func selectErrorRaw(stderrLines, stdoutLines []string) string {
	candidates := stderrLines
	if len(candidates) == 0 {
		candidates = stdoutLines
	}
	if len(candidates) == 0 {
		return ""
	}

	var nonProgress []string
	for _, line := range candidates {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !isProgressOnlyLine(line) {
			nonProgress = append(nonProgress, line)
		}
	}
	if len(nonProgress) > 0 {
		return strings.Join(nonProgress, "\n")
	}

	for i := len(candidates) - 1; i >= 0; i-- {
		if strings.TrimSpace(candidates[i]) != "" {
			return candidates[i]
		}
	}
	return ""
}

func isProgressOnlyLine(line string) bool {
	lower := strings.ToLower(line)
	for _, kw := range []string{"error", "fail", "denied", "unauthorized", "not found", "cannot", "unable", "invalid", "refused", "timeout", "conflict", "exists"} {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return percentRe.MatchString(line) || sizeRatioRe.MatchString(line) || asciiBarRe.MatchString(line)
}

func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

func classifyWSLCError(raw string, fallback error) error {
	trimmedRaw := strings.TrimSpace(raw)
	if trimmedRaw == "" {
		return fallback
	}

	rawLines := strings.Split(trimmedRaw, "\n")
	var allLines []string
	var nonLayerLines []string
	for _, l := range rawLines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		allLines = append(allLines, t)
		if !layerIDRe.MatchString(t) {
			nonLayerLines = append(nonLayerLines, t)
		}
	}

	fullMsg := strings.Join(allLines, "\n")
	if fullMsg == "" {
		return fallback
	}

	classifySource := nonLayerLines
	if len(classifySource) == 0 {
		classifySource = allLines
	}
	lower := strings.ToLower(strings.Join(classifySource, "\n"))

	switch {
	case (strings.Contains(lower, "error_already_exists") || strings.Contains(lower, "already exists")) &&
		!strings.Contains(lower, "denied") && !strings.Contains(lower, "unauthorized"):
		return fmt.Errorf("%w: %s", ErrAlreadyExists, fullMsg)
	case strings.Contains(lower, "error_not_found"),
		strings.Contains(lower, "error_file_not_found"),
		strings.Contains(lower, "not found"),
		strings.Contains(lower, "no such"),
		strings.Contains(lower, "does not exist"),
		strings.Contains(lower, "cannot find"):
		return fmt.Errorf("%w: %s", ErrNotFound, fullMsg)
	default:
		return errors.New(fullMsg)
	}
}
