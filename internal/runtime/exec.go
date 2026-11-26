package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Error struct {
	message string
	code    string
}

var (
	ErrAlreadyExists = errors.New("Resource already exists")
	ErrNotFound      = errors.New("Resource not found")
)

func RunWSLC(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "wslc.exe", args...).CombinedOutput()
	raw := strings.TrimSpace(string(out))

	if err != nil {
		return "", classifyWSLCError(raw, err)
	}

	return raw, nil
}

func classifyWSLCError(raw string, fallback error) error {
	if raw == "" {
		return fallback
	}

	errRaw := strings.Split(raw, "\n")
	headline := strings.TrimSpace(errRaw[0])

	var codeLine string
	if len(errRaw) > 1 {
		codeLine = strings.TrimSpace(errRaw[1])
	}

	switch {
	case strings.Contains(codeLine, "ERROR_ALREADY_EXISTS"):
		return fmt.Errorf("%w: %s", ErrAlreadyExists, headline)
	default:
		return errors.New(headline)
	}
}
