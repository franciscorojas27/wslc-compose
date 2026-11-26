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
	"strings"
	"sync"
)

const (
	maxStreamLineBytes     = 1024 * 1024
	maxBufferedStreamLines = 256
)

var (
	ErrAlreadyExists = errors.New("resource already exists")
	ErrNotFound      = errors.New("resource not found")
	ansiEscapeRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
)

func RunWSLC(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "wslc.exe", args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
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

	if stdout != "" {
		return stdout, nil
	}
	return stderr, nil
}

func RunWSLCInteractive(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "wslc.exe", args...)
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
	cmd := exec.CommandContext(ctx, "wslc.exe", args...)

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

	var (
		mu          sync.Mutex
		stdoutLines []string
		stderrLines []string
		wg          sync.WaitGroup
	)

	consume := func(r io.Reader, isStderr bool) {
		defer wg.Done()
		readStreamLines(r, func(rawLine string) {
			line := strings.TrimSpace(stripANSI(rawLine))
			if line == "" {
				return
			}
			mu.Lock()
			if isStderr {
				stderrLines = appendBoundedLine(stderrLines, line, maxBufferedStreamLines)
			} else {
				stdoutLines = appendBoundedLine(stdoutLines, line, maxBufferedStreamLines)
			}
			if onLine != nil {
				onLine(line)
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
		return "", classifyWSLCError(selectErrorRaw(stderrLines, stdoutLines), waitErr)
	}

	return strings.TrimSpace(strings.Join(stdoutLines, "\n")), nil
}

func readStreamLines(r io.Reader, handle func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	chunk := make([]byte, 32*1024)
	var lineBuf []byte
	skipLeadingLF := false

	flush := func() {
		if len(lineBuf) == 0 {
			return
		}
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
					lineBuf = append(lineBuf, data[start:i]...)
					flush()
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
					flush()
					start = i
				}
			}

			if start < len(data) {
				lineBuf = append(lineBuf, data[start:]...)
				if len(lineBuf) >= maxStreamLineBytes {
					flush()
				}
			}
		}

		if err != nil {
			flush()
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
		if !isProgressOnlyLine(line) {
			nonProgress = append(nonProgress, line)
		}
	}
	if len(nonProgress) > 0 {
		return strings.Join(nonProgress, "\n")
	}

	return candidates[len(candidates)-1]
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
	if raw == "" {
		return fallback
	}

	errRaw := strings.Split(raw, "\n")
	headline := strings.TrimSpace(errRaw[0])
	lower := strings.ToLower(raw)

	switch {
	case strings.Contains(lower, "error_already_exists"),
		strings.Contains(lower, "already exists"):
		return fmt.Errorf("%w: %s", ErrAlreadyExists, headline)
	case strings.Contains(lower, "error_not_found"),
		strings.Contains(lower, "error_file_not_found"),
		strings.Contains(lower, "not found"),
		strings.Contains(lower, "no such"),
		strings.Contains(lower, "does not exist"),
		strings.Contains(lower, "cannot find"):
		return fmt.Errorf("%w: %s", ErrNotFound, headline)
	default:
		return errors.New(headline)
	}
}
