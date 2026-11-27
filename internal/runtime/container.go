package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"wslc-compose/internal/config"
)

const (
	labelComposeProject    = "com.wslc.compose.project"
	labelComposeService    = "com.wslc.compose.service"
	labelComposeConfigHash = "com.wslc.compose.config-hash"
)

type ContainerInfo struct {
	ID     string
	Name   string
	Image  string
	Status string
	Ports  string
	Labels map[string]string
}

type ContainerState struct {
	Exists     bool
	Status     string
	Health     string
	ExitCode   int
	Image      string
	ConfigHash string
}

type containerRunOverrides struct {
	Detach        bool
	AutoRemove    bool
	SkipRestart   bool
	Interactive   bool
	AllocateTTY   bool
	SkipPorts     bool
	Workdir       string
	User          string
	Entrypoint    string
	HasEntrypoint bool
	Env           []string
	Command       []string
	HasCommand    bool
}

func StartContainer(ctx context.Context, name string, service config.Service) (string, error) {
	image := service.Image
	if !service.Build.IsZero() {
		if image == "" {
			image = name + ":latest"
		}
		if err := BuildImage(ctx, image, service.Build); err != nil {
			return "", err
		}
	} else if image != "" {
		if _, err := EnsureImage(ctx, image); err != nil {
			return "", err
		}
	}

	if image == "" {
		return "", fmt.Errorf("service %q has neither image nor build specified", name)
	}

	return runContainer(ctx, config.ComposeFile{}, name, name, service, image)
}

func runContainer(ctx context.Context, cfg config.ComposeFile, serviceName string, containerName string, service config.Service, image string) (string, error) {
	if err := validateServiceBindMounts(cfg, serviceName, service); err != nil {
		return "", err
	}

	if err := StopAndRemoveContainer(ctx, containerName); err != nil {
		return "", err
	}

	attachments := resolveNetworkAttachments(cfg, serviceName, service)
	if len(attachments) <= 1 {
		var primary *NetworkAttachment
		if len(attachments) == 1 {
			primary = &attachments[0]
		}
		args := buildContainerArgs("run", cfg, serviceName, containerName, service, image, primary, containerRunOverrides{
			Detach: true,
		})
		res, err := RunWSLC(ctx, args...)
		if err != nil {
			return "", fmt.Errorf("failed to start container %q: %w", containerName, err)
		}
		return strings.TrimSpace(res), nil
	}

	createArgs := buildContainerCreateArgs(cfg, serviceName, containerName, service, image, &attachments[0], containerRunOverrides{})
	res, err := RunWSLC(ctx, createArgs...)
	if err != nil {
		return "", fmt.Errorf("failed to create container %q: %w", containerName, err)
	}

	target := strings.TrimSpace(res)
	if target == "" {
		target = containerName
	}

	for _, att := range attachments[1:] {
		if err := ConnectNetwork(ctx, att, target); err != nil {
			return "", err
		}
	}

	if err := StartExistingContainer(ctx, target); err != nil {
		return "", err
	}

	return target, nil
}

func validateServiceBindMounts(cfg config.ComposeFile, serviceName string, svc config.Service) error {
	for _, mount := range svc.Volumes {
		hostPath, isBind := resolveBindMountSource(cfg, mount)
		if !isBind {
			continue
		}
		if goruntime.GOOS == "windows" && strings.HasPrefix(hostPath, "/") {
			continue
		}
		if _, err := os.Stat(hostPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("bind mount source %q for service %q does not exist", hostPath, serviceName)
			}
			return fmt.Errorf("failed to inspect bind mount source %q for service %q: %w", hostPath, serviceName, err)
		}
	}
	return nil
}

func resolveBindMountSource(cfg config.ComposeFile, mount config.VolumeMount) (string, bool) {
	if mount.Source == "" || strings.EqualFold(mount.Type, "tmpfs") {
		return "", false
	}
	_, isNamed := cfg.Volumes[mount.Source]
	if strings.EqualFold(mount.Type, "bind") {
		return cfg.ResolvePath(mount.Source), true
	}
	if isNamed {
		return "", false
	}
	if isHostBindPath(mount.Source) || looksLikeFileTarget(mount.Target) || looksLikeFileTarget(mount.Source) {
		return cfg.ResolvePath(mount.Source), true
	}
	return "", false
}

func isHostBindPath(source string) bool {
	if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "\\") || strings.Contains(source, "/") || strings.Contains(source, "\\") {
		return true
	}
	return len(source) >= 2 && source[1] == ':'
}

func looksLikeFileTarget(path string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	switch ext {
	case ".conf", ".json", ".yml", ".yaml", ".env", ".crt", ".key":
		return true
	default:
		return false
	}
}

func defaultProjectNetworkName(cfg config.ComposeFile) string {
	if _, exists := cfg.Networks["default"]; exists {
		return cfg.NetworkName("default")
	}
	return fmt.Sprintf("%s_default", cfg.ProjectName())
}

func needsDefaultNetwork(cfg config.ComposeFile, services map[string]config.Service) bool {
	if cfg.Name == "" && cfg.BaseDir == "" && cfg.ProjectDir == "" && len(services) == 0 {
		return false
	}
	for _, svc := range services {
		if len(svc.Networks) == 0 {
			return true
		}
	}
	return false
}

func withDefaultNetwork(cfg config.ComposeFile, services map[string]config.Service) config.ComposeFile {
	if !needsDefaultNetwork(cfg, services) {
		return cfg
	}
	if _, exists := cfg.Networks["default"]; exists {
		return cfg
	}
	out := cfg
	nets := make(map[string]config.Network, len(cfg.Networks)+1)
	for k, v := range cfg.Networks {
		nets[k] = v
	}
	nets["default"] = config.Network{}
	out.Networks = nets
	return out
}

func resolveNetworkAttachments(cfg config.ComposeFile, serviceName string, service config.Service) []NetworkAttachment {
	if len(service.Networks) == 0 {
		if cfg.Name == "" && cfg.BaseDir == "" && cfg.ProjectDir == "" && len(cfg.Services) == 0 {
			return nil
		}
		var aliases []string
		if serviceName != "" {
			aliases = []string{serviceName}
		}
		return []NetworkAttachment{{
			Network: defaultProjectNetworkName(cfg),
			Aliases: aliases,
		}}
	}

	netKeys := service.Networks.SortedNames()
	attachments := make([]NetworkAttachment, 0, len(netKeys))

	for _, key := range netKeys {
		netCfg := service.Networks[key]
		aliasSet := make(map[string]bool)
		if serviceName != "" {
			aliasSet[serviceName] = true
		}
		for _, alias := range netCfg.Aliases {
			if alias != "" {
				aliasSet[alias] = true
			}
		}
		aliases := make([]string, 0, len(aliasSet))
		for alias := range aliasSet {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)

		attachments = append(attachments, NetworkAttachment{
			Network: cfg.NetworkName(key),
			Aliases: aliases,
			IPv4:    netCfg.IPv4Address,
		})
	}

	return attachments
}

func computeServiceConfigHash(cfg config.ComposeFile, serviceName string, svc config.Service, image string) string {
	payload := struct {
		ServiceName string
		Image       string
		Service     config.Service
	}{
		ServiceName: serviceName,
		Image:       image,
		Service:     svc,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func buildContainerRunArgs(cfg config.ComposeFile, serviceName string, containerName string, service config.Service, image string, ov containerRunOverrides) []string {
	attachments := resolveNetworkAttachments(cfg, serviceName, service)
	var primary *NetworkAttachment
	if len(attachments) > 0 {
		primary = &attachments[0]
	}
	return buildContainerArgs("run", cfg, serviceName, containerName, service, image, primary, ov)
}

func buildContainerCreateArgs(cfg config.ComposeFile, serviceName string, containerName string, service config.Service, image string, primaryNet *NetworkAttachment, ov containerRunOverrides) []string {
	return buildContainerArgs("create", cfg, serviceName, containerName, service, image, primaryNet, ov)
}

func buildContainerArgs(subcmd string, cfg config.ComposeFile, serviceName string, containerName string, service config.Service, image string, primaryNet *NetworkAttachment, ov containerRunOverrides) []string {
	args := []string{subcmd}

	if subcmd == "run" && ov.Detach {
		args = append(args, "-d")
	}
	if subcmd == "run" && ov.AutoRemove {
		args = append(args, "--rm")
	}
	if containerName != "" {
		args = append(args, "--name", containerName)
	}

	if service.Restart != "" && !ov.AutoRemove && !ov.SkipRestart {
		args = append(args, "--restart", service.Restart)
	}

	workdir := service.WorkingDir
	if ov.Workdir != "" {
		workdir = ov.Workdir
	}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}

	user := service.User
	if ov.User != "" {
		user = ov.User
	}
	if user != "" {
		args = append(args, "-u", user)
	}

	if service.Hostname != "" {
		args = append(args, "-h", service.Hostname)
	}
	if service.Privileged {
		args = append(args, "--privileged")
	}
	if service.Init {
		args = append(args, "--init")
	}
	if service.ShmSize != "" {
		args = append(args, "--shm-size", service.ShmSize)
	}
	if service.TTY || ov.AllocateTTY {
		args = append(args, "-t")
	}
	if service.StdinOpen || ov.Interactive {
		args = append(args, "-i")
	}

	for _, envFile := range service.EnvFile {
		if envFile == "" {
			continue
		}
		args = append(args, "--env-file", cfg.ResolvePath(envFile))
	}

	if !ov.SkipPorts {
		for _, port := range service.Ports {
			args = append(args, "-p", port)
		}
	}

	for _, mount := range service.Volumes {
		if mount.Target == "" {
			continue
		}
		if strings.EqualFold(mount.Type, "tmpfs") {
			spec := mount.Target
			if mount.ReadOnly {
				spec += ":ro"
			}
			args = append(args, "--tmpfs", spec)
			continue
		}
		args = append(args, "-v", formatVolumeMount(cfg, mount))
	}

	envMap := make(map[string]string, len(service.Environment)+len(ov.Env))
	for k, v := range service.Environment {
		envMap[k] = v
	}
	var bareEnv []string
	for _, entry := range ov.Env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			envMap[k] = v
		} else if entry != "" {
			bareEnv = append(bareEnv, entry)
		}
	}
	if len(envMap) > 0 {
		envKeys := make([]string, 0, len(envMap))
		for k := range envMap {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			args = append(args, "-e", k+"="+envMap[k])
		}
	}
	for _, entry := range bareEnv {
		args = append(args, "-e", entry)
	}

	labels := make(map[string]string, len(service.Labels)+3)
	for k, v := range service.Labels {
		labels[k] = v
	}
	if proj := cfg.ProjectName(); proj != "" && cfg.Name != "" {
		labels[labelComposeProject] = proj
	}
	if serviceName != "" {
		labels[labelComposeService] = serviceName
	}
	if !ov.SkipRestart && serviceName != "" {
		if hash := computeServiceConfigHash(cfg, serviceName, service, image); hash != "" {
			labels[labelComposeConfigHash] = hash
		}
	}
	if len(labels) > 0 {
		labelKeys := make([]string, 0, len(labels))
		for k := range labels {
			labelKeys = append(labelKeys, k)
		}
		sort.Strings(labelKeys)
		for _, k := range labelKeys {
			args = append(args, "-l", k+"="+labels[k])
		}
	}

	for _, host := range service.ExtraHosts {
		if host != "" {
			args = append(args, "--add-host", host)
		}
	}

	for _, dns := range service.DNS {
		if dns != "" {
			args = append(args, "--dns", dns)
		}
	}

	if service.HealthCheck != nil {
		hc := service.HealthCheck
		if hc.Disable || (len(hc.Test) > 0 && strings.EqualFold(hc.Test[0], "NONE")) {
			args = append(args, "--no-healthcheck")
		} else if len(hc.Test) > 0 {
			cmdStr := formatHealthCmd(hc.Test)
			if cmdStr != "" {
				args = append(args, "--health-cmd", cmdStr)
			}
			if hc.Interval != "" {
				args = append(args, "--health-interval", hc.Interval)
			}
			if hc.Timeout != "" {
				args = append(args, "--health-timeout", hc.Timeout)
			}
			if hc.Retries > 0 {
				args = append(args, "--health-retries", strconv.Itoa(hc.Retries))
			}
			if hc.StartPeriod != "" {
				args = append(args, "--health-start-period", hc.StartPeriod)
			}
		}
	}

	if primaryNet != nil && primaryNet.Network != "" {
		args = append(args, "--network", primaryNet.Network)
		if primaryNet.IPv4 != "" {
			args = append(args, "--ip", primaryNet.IPv4)
		}
		for _, alias := range primaryNet.Aliases {
			if alias != "" {
				args = append(args, "--network-alias", alias)
			}
		}
	}

	var trailingArgs []string
	if ov.HasEntrypoint {
		args = append(args, "--entrypoint", ov.Entrypoint)
	} else if len(service.Entrypoint) > 0 {
		args = append(args, "--entrypoint", service.Entrypoint[0])
		if len(service.Entrypoint) > 1 {
			trailingArgs = append(trailingArgs, service.Entrypoint[1:]...)
		}
	}

	if ov.HasCommand {
		trailingArgs = append(trailingArgs, ov.Command...)
	} else if !ov.HasEntrypoint && len(service.Command) > 0 {
		trailingArgs = append(trailingArgs, service.Command...)
	}

	args = append(args, image)
	args = append(args, trailingArgs...)
	return args
}

func formatVolumeMount(cfg config.ComposeFile, mount config.VolumeMount) string {
	source := mount.Source
	if source != "" {
		if hostPath, isBind := resolveBindMountSource(cfg, mount); isBind {
			source = hostPath
		} else if _, isNamed := cfg.Volumes[source]; isNamed {
			source = cfg.VolumeName(source)
		}
	}

	var spec string
	if source != "" {
		spec = source + ":" + mount.Target
	} else {
		spec = mount.Target
	}
	if mount.ReadOnly {
		spec += ":ro"
	}
	return spec
}

func formatHealthCmd(test config.HealthCheckTest) string {
	if len(test) == 0 {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(test[0])) {
	case "NONE":
		return ""
	case "CMD-SHELL":
		if len(test) > 1 {
			return strings.Join(test[1:], " ")
		}
		return ""
	case "CMD":
		if len(test) > 1 {
			quoted := make([]string, 0, len(test)-1)
			for _, arg := range test[1:] {
				quoted = append(quoted, shellQuoteArg(arg))
			}
			return strings.Join(quoted, " ")
		}
		return ""
	default:
		quoted := make([]string, 0, len(test))
		for _, arg := range test {
			quoted = append(quoted, shellQuoteArg(arg))
		}
		return strings.Join(quoted, " ")
	}
}

func shellQuoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	needsQuote := false
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '=' || c == '+' || c == ',' || c == '@' {
			continue
		}
		needsQuote = true
		break
	}
	if !needsQuote {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func StartExistingContainer(ctx context.Context, name string) error {
	if _, err := RunWSLC(ctx, "start", name); err != nil {
		return fmt.Errorf("failed to start container %q: %w", name, err)
	}
	return nil
}

func StopContainer(ctx context.Context, name string, timeout int) error {
	args := []string{"stop"}
	if timeout >= 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	args = append(args, name)

	if _, err := RunWSLC(ctx, args...); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to stop container %q: %w", name, err)
	}
	return nil
}

func KillContainer(ctx context.Context, name string, signal string) error {
	sig := strings.TrimSpace(signal)
	if sig == "" {
		sig = "SIGKILL"
	}
	if _, err := RunWSLC(ctx, "kill", "-s", sig, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to kill container %q: %w", name, err)
	}
	return nil
}

func RemoveContainer(ctx context.Context, name string, force bool, removeVolumes bool) error {
	args := []string{"remove"}
	if force {
		args = append(args, "-f")
	}
	if removeVolumes {
		args = append(args, "-v")
	}
	args = append(args, name)

	if _, err := RunWSLC(ctx, args...); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to remove container %q: %w", name, err)
	}
	return nil
}

func StopAndRemoveContainer(ctx context.Context, name string) error {
	return RemoveContainer(ctx, name, true, false)
}
