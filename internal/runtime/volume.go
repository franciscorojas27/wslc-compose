package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"wslc-compose/internal/config"
)

func ResolveVolumeName(name string, vol config.Volume) string {
	if vol.Name != "" {
		return vol.Name
	}
	return name
}

func CreateVolume(ctx context.Context, name string, vol config.Volume) (string, error) {
	res, _, err := createVolume(ctx, name, vol)
	return res, err
}

func buildVolumeCreateArgs(target string, vol config.Volume) []string {
	args := []string{"volume", "create"}

	driver := strings.TrimSpace(vol.Driver)
	if driver != "" && !strings.EqualFold(driver, "local") {
		args = append(args, "-d", driver)
	}

	if len(vol.DriverOpts) > 0 {
		keys := make([]string, 0, len(vol.DriverOpts))
		for k := range vol.DriverOpts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-o", fmt.Sprintf("%s=%s", k, vol.DriverOpts[k]))
		}
	}

	if len(vol.Labels) > 0 {
		keys := make([]string, 0, len(vol.Labels))
		for k := range vol.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-l", fmt.Sprintf("%s=%s", k, vol.Labels[k]))
		}
	}

	args = append(args, target)
	return args
}

func createVolume(ctx context.Context, name string, vol config.Volume) (string, bool, error) {
	target := ResolveVolumeName(name, vol)
	if vol.External {
		return target, false, nil
	}

	args := buildVolumeCreateArgs(target, vol)

	res, err := RunWSLC(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return target, false, nil
		}
		return "", false, fmt.Errorf("failed to create volume %q: %w", target, err)
	}

	return res, true, nil
}

func RemoveVolume(ctx context.Context, name string, vol config.Volume) (string, error) {
	target := ResolveVolumeName(name, vol)
	if vol.External {
		return target, nil
	}

	res, err := RunWSLC(ctx, "volume", "rm", target)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to remove volume %q: %w", target, err)
	}

	return res, nil
}

func ListVolumes(ctx context.Context) (map[string]bool, error) {
	out, err := RunWSLC(ctx, "volume", "ls", "-q")
	if err == nil {
		lines := strings.Split(out, "\n")
		if len(lines) == 0 || !isVolumeTableHeader(lines[0]) {
			existing := make(map[string]bool)
			for _, line := range lines {
				name := strings.TrimSpace(line)
				if name != "" {
					existing[name] = true
				}
			}
			return existing, nil
		}
		return parseVolumeTable(out), nil
	}

	out, err = RunWSLC(ctx, "volume", "ls")
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}

	return parseVolumeTable(out), nil
}

func isVolumeTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	cols := multiSpaceRe.Split(trimmed, -1)
	if len(cols) == 0 {
		return false
	}
	for _, col := range cols {
		switch strings.ToUpper(strings.TrimSpace(col)) {
		case "DRIVER", "VOLUME NAME", "NAME", "CONTROLADOR", "NOMBRE DE VOLUMEN", "NOMBRE DEL VOLUMEN", "NOMBRE":
		default:
			return false
		}
	}
	return true
}

func parseVolumeTable(raw string) map[string]bool {
	existing := make(map[string]bool)
	lines := strings.Split(raw, "\n")
	startIdx := 0
	nameCol := -1

	if len(lines) > 0 && isVolumeTableHeader(lines[0]) {
		startIdx = 1
		headers := multiSpaceRe.Split(strings.TrimSpace(lines[0]), -1)
		for i, h := range headers {
			uh := strings.ToUpper(strings.TrimSpace(h))
			if uh == "VOLUME NAME" || uh == "NAME" || uh == "NOMBRE DE VOLUMEN" || uh == "NOMBRE DEL VOLUMEN" || uh == "NOMBRE" {
				nameCol = i
				break
			}
		}
	}

	for _, line := range lines[startIdx:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		cols := multiSpaceRe.Split(trimmed, -1)
		if nameCol >= 0 && nameCol < len(cols) {
			existing[strings.TrimSpace(cols[nameCol])] = true
			continue
		}
		if len(cols) >= 2 {
			existing[strings.TrimSpace(cols[1])] = true
			continue
		}
		existing[strings.TrimSpace(cols[0])] = true
	}

	return existing
}

func VolumeExists(ctx context.Context, name string, vol config.Volume) bool {
	target := ResolveVolumeName(name, vol)
	if _, err := RunWSLC(ctx, "volume", "inspect", target); err == nil {
		return true
	} else if errors.Is(err, ErrNotFound) {
		return false
	}

	existing, err := ListVolumes(ctx)
	if err != nil {
		return false
	}
	return existing[target]
}
