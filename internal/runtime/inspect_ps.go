package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"wslc-compose/internal/config"
)

func InspectContainerState(ctx context.Context, name string) (ContainerState, error) {
	out, err := RunWSLC(ctx, "inspect", "--format", "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}|{{.State.ExitCode}}", name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ContainerState{Exists: false}, nil
		}
		containers, listErr := ListContainers(ctx)
		if listErr != nil {
			return ContainerState{}, err
		}
		for _, c := range containers {
			if c.Name == name {
				return parseStateFromStatusText(c.Status), nil
			}
		}
		return ContainerState{Exists: false}, nil
	}

	if strings.Contains(out, "{{.") {
		containers, listErr := ListContainers(ctx)
		if listErr != nil {
			return ContainerState{Exists: true}, nil
		}
		for _, c := range containers {
			if c.Name == name {
				return parseStateFromStatusText(c.Status), nil
			}
		}
		return ContainerState{Exists: false}, nil
	}

	parts := strings.Split(strings.TrimSpace(out), "|")
	st := ContainerState{Exists: true}
	if len(parts) > 0 {
		st.Status = strings.ToLower(strings.TrimSpace(parts[0]))
	}
	if len(parts) > 1 {
		st.Health = strings.ToLower(strings.TrimSpace(parts[1]))
	}
	if len(parts) > 2 {
		if code, convErr := strconv.Atoi(strings.TrimSpace(parts[2])); convErr == nil {
			st.ExitCode = code
		}
	}
	return st, nil
}

func parseStateFromStatusText(status string) ContainerState {
	lower := strings.ToLower(strings.TrimSpace(status))
	st := ContainerState{Exists: true}

	switch {
	case strings.HasPrefix(lower, "up"), strings.HasPrefix(lower, "running"):
		st.Status = "running"
	case strings.HasPrefix(lower, "exited"):
		st.Status = "exited"
		if start := strings.Index(lower, "("); start >= 0 {
			if end := strings.Index(lower[start+1:], ")"); end >= 0 {
				if code, err := strconv.Atoi(lower[start+1 : start+1+end]); err == nil {
					st.ExitCode = code
				}
			}
		}
	case strings.HasPrefix(lower, "created"):
		st.Status = "created"
	case strings.HasPrefix(lower, "restarting"):
		st.Status = "restarting"
	case strings.HasPrefix(lower, "paused"):
		st.Status = "paused"
	case strings.HasPrefix(lower, "dead"):
		st.Status = "dead"
	default:
		st.Status = lower
	}

	switch {
	case strings.Contains(lower, "(healthy)"):
		st.Health = "healthy"
	case strings.Contains(lower, "(unhealthy)"):
		st.Health = "unhealthy"
	case strings.Contains(lower, "(health: starting)"):
		st.Health = "starting"
	}

	return st
}

func WaitForCondition(ctx context.Context, containerName string, svc config.Service, condition string) error {
	switch condition {
	case "", "service_started":
		return nil
	case "service_completed_successfully":
		return waitForCompleted(ctx, containerName)
	case "service_healthy":
		return waitForHealthy(ctx, containerName, svc)
	default:
		return fmt.Errorf("unsupported wait condition %q", condition)
	}
}

func waitForCompleted(ctx context.Context, containerName string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		st, err := InspectContainerState(ctx, containerName)
		if err != nil {
			return err
		}
		if !st.Exists {
			return fmt.Errorf("container %q no longer exists while waiting for completion", containerName)
		}
		if st.Status == "exited" {
			if st.ExitCode == 0 {
				return nil
			}
			return fmt.Errorf("container %q exited with code %d", containerName, st.ExitCode)
		}
		if st.Status == "dead" {
			return fmt.Errorf("container %q died while waiting for completion", containerName)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForHealthy(ctx context.Context, containerName string, svc config.Service) error {
	interval := 1 * time.Second
	retries := 30
	var startPeriod time.Duration

	if svc.HealthCheck != nil {
		if d, err := time.ParseDuration(svc.HealthCheck.Interval); err == nil && d > 0 {
			interval = d
		}
		if svc.HealthCheck.Retries > 0 {
			retries = svc.HealthCheck.Retries
		}
		if d, err := time.ParseDuration(svc.HealthCheck.StartPeriod); err == nil && d > 0 {
			startPeriod = d
		}
	}

	deadline := time.Now().Add(startPeriod)
	consecutiveFailures := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		st, err := InspectContainerState(ctx, containerName)
		if err != nil {
			return err
		}
		if !st.Exists {
			return fmt.Errorf("container %q no longer exists while waiting for healthy state", containerName)
		}
		if st.Status == "exited" || st.Status == "dead" {
			return fmt.Errorf("container %q exited unexpectedly (code %d) while waiting to become healthy", containerName, st.ExitCode)
		}
		if st.Health == "healthy" {
			return nil
		}
		if st.Health == "unhealthy" {
			return fmt.Errorf("container %q is unhealthy", containerName)
		}

		if st.Health == "" {
			if svc.HealthCheck == nil || svc.HealthCheck.Disable || len(svc.HealthCheck.Test) == 0 {
				if st.Status == "running" {
					return nil
				}
			} else if st.Status == "running" {
				if runHealthProbe(ctx, containerName, svc.HealthCheck.Test) == nil {
					return nil
				}
				if time.Now().After(deadline) {
					consecutiveFailures++
					if consecutiveFailures >= retries {
						return fmt.Errorf("container %q failed healthcheck after %d retries", containerName, retries)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runHealthProbe(ctx context.Context, containerName string, test config.HealthCheckTest) error {
	if len(test) == 0 {
		return nil
	}
	args := []string{"exec", containerName}
	switch strings.ToUpper(test[0]) {
	case "CMD-SHELL":
		if len(test) < 2 {
			return nil
		}
		args = append(args, "sh", "-c", strings.Join(test[1:], " "))
	case "CMD":
		if len(test) < 2 {
			return nil
		}
		args = append(args, test[1:]...)
	default:
		args = append(args, test...)
	}

	_, err := RunWSLC(ctx, args...)
	return err
}

func ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	out, err := RunWSLC(ctx, "ps", "-a", "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Labels}}")
	if err == nil && !strings.Contains(out, "{{.") && (out == "" || strings.Contains(out, "\t")) {
		return parseFormattedContainers(out), nil
	}

	out, err = RunWSLC(ctx, "ps", "-a", "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}")
	if err == nil && !strings.Contains(out, "{{.") && (out == "" || strings.Contains(out, "\t")) {
		return parseFormattedContainers(out), nil
	}

	out, err = RunWSLC(ctx, "ps", "-a")
	if err != nil {
		out, err = RunWSLC(ctx, "container", "ls", "-a")
		if err != nil {
			return nil, fmt.Errorf("failed to list containers: %w", err)
		}
	}

	return parseTableContainers(out), nil
}

func parseFormattedContainers(raw string) []ContainerInfo {
	var list []ContainerInfo
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		info := ContainerInfo{
			ID:   strings.TrimSpace(parts[0]),
			Name: strings.TrimPrefix(strings.TrimSpace(parts[1]), "/"),
		}
		if len(parts) > 2 {
			info.Image = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 {
			info.Status = strings.TrimSpace(parts[3])
		}
		if len(parts) > 4 {
			info.Ports = strings.TrimSpace(parts[4])
		}
		if len(parts) > 5 {
			info.Labels = parseCommaLabels(parts[5])
		}
		list = append(list, info)
	}
	return list
}

func parseCommaLabels(raw string) map[string]string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, pair := range strings.Split(trimmed, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && k != "" {
			labels[k] = v
		}
	}
	return labels
}

func parseTableContainers(raw string) []ContainerInfo {
	lines := strings.Split(raw, "\n")
	if len(lines) < 2 {
		return nil
	}

	headerLine := strings.TrimRight(lines[0], "\r")
	headers := multiSpaceRe.Split(strings.TrimSpace(headerLine), -1)
	if len(headers) == 0 {
		return nil
	}

	offsets := findHeaderRuneOffsets(headerLine, headers)

	var list []ContainerInfo
	for _, rawLine := range lines[1:] {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		cols := make([]string, len(headers))
		splitCols := multiSpaceRe.Split(strings.TrimSpace(line), -1)
		if len(splitCols) == len(headers) {
			cols = splitCols
		} else {
			lineRunes := []rune(line)
			for i := range headers {
				start := offsets[i]
				if start >= len(lineRunes) {
					continue
				}
				end := len(lineRunes)
				if i+1 < len(headers) && offsets[i+1] < end {
					end = offsets[i+1]
				}
				cols[i] = strings.TrimSpace(string(lineRunes[start:end]))
			}
		}

		var info ContainerInfo
		for i, h := range headers {
			val := strings.TrimSpace(cols[i])
			switch strings.ToUpper(strings.TrimSpace(h)) {
			case "CONTAINER ID", "ID", "ID. DE CONTENEDOR", "ID DE CONTENEDOR":
				info.ID = val
			case "NAMES", "NAME", "NOMBRES", "NOMBRE":
				info.Name = strings.TrimPrefix(val, "/")
			case "IMAGE", "IMAGEN":
				info.Image = val
			case "STATUS", "STATE", "ESTADO":
				info.Status = val
			case "PORTS", "PUERTOS":
				info.Ports = val
			}
		}

		if info.Name != "" || info.ID != "" {
			list = append(list, info)
		}
	}

	return list
}

func findHeaderRuneOffsets(headerLine string, headers []string) []int {
	headerRunes := []rune(headerLine)
	offsets := make([]int, len(headers))
	searchFrom := 0
	for i, h := range headers {
		hRunes := []rune(h)
		idx := indexRunes(headerRunes[searchFrom:], hRunes)
		if idx < 0 {
			offsets[i] = searchFrom
		} else {
			offsets[i] = searchFrom + idx
			searchFrom = offsets[i] + len(hRunes)
		}
	}
	return offsets
}

func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	limit := len(haystack) - len(needle)
	for i := 0; i <= limit; i++ {
		matched := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}
