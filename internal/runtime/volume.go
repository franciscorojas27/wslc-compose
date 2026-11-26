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

func createVolume(ctx context.Context, name string, vol config.Volume) (string, bool, error) {
	target := ResolveVolumeName(name, vol)
	if vol.External {
		return target, false, nil
	}

	args := []string{"volume", "create"}

	if vol.Driver != "" && vol.Driver != "local" {
		args = append(args, "--driver", vol.Driver)
	}

	if len(vol.Labels) > 0 {
		keys := make([]string, 0, len(vol.Labels))
		for k := range vol.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--label", fmt.Sprintf("%s=%s", k, vol.Labels[k]))
		}
	}

	args = append(args, target)

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
		firstLine := strings.ToUpper(strings.Split(out, "\n")[0])
		isTable := strings.Contains(firstLine, "DRIVER") || strings.Contains(firstLine, "CONTROLADOR") || strings.Contains(firstLine, "VOLUME NAME") || strings.Contains(firstLine, "NOMBRE")
		if !isTable {
			existing := make(map[string]bool)
			for _, line := range strings.Split(out, "\n") {
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

func parseVolumeTable(raw string) map[string]bool {
	existing := make(map[string]bool)
	lines := strings.Split(raw, "\n")
	startIdx := 0
	nameCol := -1

	if len(lines) > 0 {
		upperHeader := strings.ToUpper(lines[0])
		if strings.Contains(upperHeader, "NAME") || strings.Contains(upperHeader, "DRIVER") || strings.Contains(upperHeader, "NOMBRE") || strings.Contains(upperHeader, "CONTROLADOR") {
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
