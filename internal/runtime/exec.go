package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func RunWSLC(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "wslc.exe", args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("wslc %s falló: %w (%s)", strings.Join(args, " "), err, text)
	}
	return text, nil
}
