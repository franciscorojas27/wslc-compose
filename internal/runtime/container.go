package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"wslc-compose/internal/config"
)

const (
	labelComposeProject = "com.wslc.compose.project"
	labelComposeService = "com.wslc.compose.service"
)

type BuildOptions struct {
	NoCache    bool
	ProjectDir string
}

type PullProgress struct {
	Percent int
	HasPct  bool
	Detail  string
	Layers  []int
}

type ContainerInfo struct {
	ID     string
	Name   string
	Image  string
	Status string
	Ports  string
	Labels map[string]string
}

type ContainerState struct {
	Exists   bool
	Status   string
	Health   string
	ExitCode int
}

type ImageInfo struct {
	Repository string
	Tag        string
	ID         string
	Size       string
}

type containerRunOverrides struct {
	Detach        bool
	AutoRemove    bool
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

var (
	percentRe    = regexp.MustCompile(`(\d{1,3})(?:\.\d+)?%`)
	sizeRatioRe  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([kmgt]?i?b)\s*/\s*(\d+(?:\.\d+)?)\s*([kmgt]?i?b)`)
	layerRatioRe = regexp.MustCompile(`\b(\d+)\s*/\s*(\d+)\b`)
	layerIDRe    = regexp.MustCompile(`^(?:sha256:)?([0-9a-fA-F]{6,64}):\s*(.+)$`)
	asciiBarRe   = regexp.MustCompile(`\[[=\->#\.\s]+\]`)
	multiSpaceRe = regexp.MustCompile(`\s{2,}`)
)

func BuildImage(ctx context.Context, tag string, build config.BuildConfig, opts ...BuildOptions) error {
	var opt BuildOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	buildContext := build.Context
	if buildContext == "" {
		buildContext = "."
	}
	if !filepath.IsAbs(buildContext) && opt.ProjectDir != "" {
		buildContext = filepath.Join(opt.ProjectDir, buildContext)
	}

	args := []string{"build", "-t", tag}

	if opt.NoCache {
		args = append(args, "--no-cache")
	}

	if build.Dockerfile != "" {
		dockerfilePath := build.Dockerfile
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(buildContext, dockerfilePath)
		}
		args = append(args, "-f", dockerfilePath)
	}

	if build.Target != "" {
		args = append(args, "--target", build.Target)
	}

	if len(build.Args) > 0 {
		keys := make([]string, 0, len(build.Args))
		for k := range build.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--build-arg", k+"="+build.Args[k])
		}
	}

	args = append(args, buildContext)

	if _, err := RunWSLCStream(ctx, nil, args...); err != nil {
		return fmt.Errorf("failed to build image %q: %w", tag, err)
	}

	return nil
}

func PullImage(ctx context.Context, image string, onProgress ...func(PullProgress)) error {
	if image == "" {
		return fmt.Errorf("image name is required")
	}

	var cb func(PullProgress)
	if len(onProgress) > 0 {
		cb = onProgress[0]
	}

	var onLine func(string)
	if cb != nil {
		tracker := newPullTracker()
		onLine = func(line string) {
			if prog, ok := tracker.update(line); ok {
				cb(prog)
			}
		}
	}

	if _, err := RunWSLCStream(ctx, onLine, "pull", image); err != nil {
		return fmt.Errorf("failed to pull image %q: %w", image, err)
	}

	return nil
}

type layerProgress struct {
	downloadCur float64
	downloadTot float64
	extractCur  float64
	extractTot  float64
	downloading bool
	downloaded  bool
	extracting  bool
	complete    bool
}

func (l *layerProgress) level() int {
	if l.complete {
		return 8
	}
	if l.extracting {
		if l.extractTot > 0 {
			ratio := l.extractCur / l.extractTot
			if ratio > 1.0 {
				ratio = 1.0
			}
			return 6 + int(ratio*1.9)
		}
		return 7
	}
	if l.downloaded {
		return 6
	}
	if l.downloadTot > 0 {
		ratio := l.downloadCur / l.downloadTot
		if ratio > 1.0 {
			ratio = 1.0
		}
		lvl := 1 + int(ratio*5)
		if lvl > 6 {
			lvl = 6
		}
		return lvl
	}
	if l.downloading {
		return 3
	}
	return 0
}

func (l *layerProgress) fraction() float64 {
	if l.complete {
		return 1.0
	}
	if l.extractTot > 0 {
		ratio := l.extractCur / l.extractTot
		if ratio > 1.0 {
			ratio = 1.0
		}
		return 0.70 + 0.30*ratio
	}
	if l.downloaded {
		return 0.70
	}
	if l.downloadTot > 0 {
		ratio := l.downloadCur / l.downloadTot
		if ratio > 1.0 {
			ratio = 1.0
		}
		return 0.70 * ratio
	}
	return 0.0
}

type pullTracker struct {
	order      []string
	layers     map[string]*layerProgress
	maxPercent int
}

func newPullTracker() *pullTracker {
	return &pullTracker{
		layers: make(map[string]*layerProgress),
	}
}

func (t *pullTracker) update(line string) (PullProgress, bool) {
	if m := layerIDRe.FindStringSubmatch(line); len(m) == 3 {
		layerID := strings.ToLower(m[1])
		rest := strings.TrimSpace(m[2])
		lowerRest := strings.ToLower(rest)

		layer, exists := t.layers[layerID]
		if !exists {
			layer = &layerProgress{}
			t.layers[layerID] = layer
			t.order = append(t.order, layerID)
		}

		curBytes, totBytes, hasSizes := extractSizePair(rest)

		switch {
		case strings.Contains(lowerRest, "pull complete"),
			strings.Contains(lowerRest, "already exists"),
			strings.Contains(lowerRest, "ya existe"):
			layer.downloading = false
			layer.downloaded = true
			layer.extracting = false
			layer.complete = true
			if layer.downloadTot > 0 {
				layer.downloadCur = layer.downloadTot
			}
			if layer.extractTot > 0 {
				layer.extractCur = layer.extractTot
			}
		case strings.Contains(lowerRest, "download complete"),
			strings.Contains(lowerRest, "verifying checksum"),
			strings.Contains(lowerRest, "descarga completa"):
			layer.downloading = false
			layer.downloaded = true
			if layer.downloadTot > 0 {
				layer.downloadCur = layer.downloadTot
			}
		case strings.Contains(lowerRest, "extracting"),
			strings.Contains(lowerRest, "extrayendo"):
			layer.downloading = false
			layer.downloaded = true
			layer.extracting = true
			if layer.downloadTot > 0 {
				layer.downloadCur = layer.downloadTot
			}
			if hasSizes && totBytes > 0 {
				layer.extractCur = curBytes
				layer.extractTot = totBytes
			}
		case strings.Contains(lowerRest, "downloading"),
			strings.Contains(lowerRest, "descargando"):
			layer.downloading = true
			if hasSizes && totBytes > 0 {
				layer.downloadCur = curBytes
				layer.downloadTot = totBytes
			}
		}

		return t.snapshot(), true
	}

	if len(t.layers) > 0 {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "status: downloaded") || strings.Contains(lower, "status: image is up to date") {
			for _, id := range t.order {
				t.layers[id].complete = true
			}
			t.maxPercent = 100
			return t.snapshot(), true
		}
		return PullProgress{}, false
	}

	p := parsePullProgress(line)
	if p.HasPct {
		if p.Percent < t.maxPercent {
			p.Percent = t.maxPercent
		} else {
			t.maxPercent = p.Percent
		}
		return p, true
	}

	return PullProgress{Percent: t.maxPercent, HasPct: true, Detail: "Resolving"}, true
}

func (t *pullTracker) snapshot() PullProgress {
	totalLayers := len(t.order)
	if totalLayers == 0 {
		return PullProgress{Percent: t.maxPercent, HasPct: true}
	}

	levels := make([]int, totalLayers)
	var sumFrac float64
	doneLayers := 0

	for i, id := range t.order {
		l := t.layers[id]
		levels[i] = l.level()
		sumFrac += l.fraction()
		if l.complete {
			doneLayers++
		}
	}

	pct := int((sumFrac / float64(totalLayers)) * 100)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if pct < t.maxPercent {
		pct = t.maxPercent
	} else {
		t.maxPercent = pct
	}

	return PullProgress{
		Percent: pct,
		HasPct:  true,
		Detail:  fmt.Sprintf("%d/%d", doneLayers, totalLayers),
		Layers:  levels,
	}
}

func extractSizePair(line string) (float64, float64, bool) {
	m := sizeRatioRe.FindStringSubmatch(line)
	if len(m) != 5 {
		return 0, 0, false
	}
	return toBytes(m[1], m[2]), toBytes(m[3], m[4]), true
}

func formatBytes(b float64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1fGB", b/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fMB", b/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.0fkB", b/1024)
	default:
		return fmt.Sprintf("%.0fB", b)
	}
}

func RemoveImage(ctx context.Context, image string) error {
	if image == "" {
		return nil
	}
	if _, err := RunWSLC(ctx, "rmi", image); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to remove image %q: %w", image, err)
	}
	return nil
}

func parsePullProgress(line string) PullProgress {
	var p PullProgress

	if m := percentRe.FindStringSubmatch(line); len(m) >= 2 {
		if val, err := strconv.Atoi(m[1]); err == nil {
			if val < 0 {
				val = 0
			}
			if val > 100 {
				val = 100
			}
			p.Percent = val
			p.HasPct = true
		}
	}

	sizeDetail := ""
	if m := sizeRatioRe.FindStringSubmatch(line); len(m) == 5 {
		sizeDetail = m[1] + strings.ToUpper(m[2]) + "/" + m[3] + strings.ToUpper(m[4])
		if !p.HasPct {
			cur := toBytes(m[1], m[2])
			tot := toBytes(m[3], m[4])
			if tot > 0 {
				val := int((cur / tot) * 100)
				if val < 0 {
					val = 0
				}
				if val > 100 {
					val = 100
				}
				p.Percent = val
				p.HasPct = true
			}
		}
	}

	lower := strings.ToLower(line)
	if !p.HasPct && (strings.Contains(lower, "layer") || strings.Contains(lower, "blob") || strings.Contains(lower, "download") || strings.Contains(lower, "extract") || strings.Contains(lower, "pull")) {
		if m := layerRatioRe.FindStringSubmatch(line); len(m) == 3 {
			cur, err1 := strconv.Atoi(m[1])
			tot, err2 := strconv.Atoi(m[2])
			if err1 == nil && err2 == nil && tot > 0 && cur <= tot {
				p.Percent = (cur * 100) / tot
				p.HasPct = true
			}
		}
	}

	if sizeDetail != "" {
		switch {
		case strings.Contains(lower, "extract"):
			p.Detail = "Extracting " + sizeDetail
		case strings.Contains(lower, "download"):
			p.Detail = "Downloading " + sizeDetail
		default:
			p.Detail = sizeDetail
		}
		return p
	}

	cleaned := asciiBarRe.ReplaceAllString(line, "")
	cleaned = percentRe.ReplaceAllString(cleaned, "")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	p.Detail = cleaned
	return p
}

func toBytes(numStr string, unit string) float64 {
	v, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(unit) {
	case "kb", "kib":
		return v * 1024
	case "mb", "mib":
		return v * 1024 * 1024
	case "gb", "gib":
		return v * 1024 * 1024 * 1024
	case "tb", "tib":
		return v * 1024 * 1024 * 1024 * 1024
	default:
		return v
	}
}

func ImageExists(ctx context.Context, image string) bool {
	if image == "" {
		return false
	}

	_, err := RunWSLC(ctx, "image", "inspect", image)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrNotFound) {
		return false
	}

	out, listErr := RunWSLC(ctx, "images")
	if listErr != nil {
		return false
	}

	return imageInList(out, image)
}

func EnsureImage(ctx context.Context, image string) (bool, error) {
	if ImageExists(ctx, image) {
		return false, nil
	}
	if err := PullImage(ctx, image); err != nil {
		return false, err
	}
	return true, nil
}

func ListImages(ctx context.Context) ([]ImageInfo, error) {
	out, err := RunWSLC(ctx, "images", "--format", "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.Size}}")
	if err == nil && !strings.Contains(out, "{{.") && (out == "" || strings.Contains(out, "\t")) {
		var list []ImageInfo
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) < 2 {
				continue
			}
			info := ImageInfo{
				Repository: strings.TrimSpace(parts[0]),
				Tag:        strings.TrimSpace(parts[1]),
			}
			if len(parts) > 2 {
				info.ID = strings.TrimSpace(parts[2])
			}
			if len(parts) > 3 {
				info.Size = strings.TrimSpace(parts[3])
			}
			list = append(list, info)
		}
		return list, nil
	}

	out, err = RunWSLC(ctx, "images")
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}

	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return nil, nil
	}

	startIdx := 0
	upperFirst := strings.ToUpper(lines[0])
	if strings.Contains(upperFirst, "REPOSITORY") || strings.Contains(upperFirst, "REPOSITORIO") {
		startIdx = 1
	}

	var list []ImageInfo
	for _, line := range lines[startIdx:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		cols := multiSpaceRe.Split(trimmed, -1)
		if len(cols) < 2 {
			continue
		}
		info := ImageInfo{
			Repository: strings.TrimSpace(cols[0]),
			Tag:        strings.TrimSpace(cols[1]),
		}
		if len(cols) > 2 {
			info.ID = strings.TrimSpace(cols[2])
		}
		if len(cols) > 4 {
			info.Size = strings.TrimSpace(cols[4])
		} else if len(cols) > 3 {
			info.Size = strings.TrimSpace(cols[3])
		}
		list = append(list, info)
	}

	return list, nil
}

func imageInList(raw string, image string) bool {
	targetRepo, targetTag := splitImageRef(image)
	targetRepo = trimRegistryPrefix(targetRepo)

	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == image {
			return true
		}
		if len(fields) >= 2 {
			repo := trimRegistryPrefix(fields[0])
			tag := fields[1]
			if repo == targetRepo && tag == targetTag {
				return true
			}
		}
	}

	return false
}

func splitImageRef(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, "latest"
}

func trimRegistryPrefix(repo string) string {
	repo = strings.TrimPrefix(repo, "docker.io/library/")
	repo = strings.TrimPrefix(repo, "docker.io/")
	return repo
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

func resolveNetworkAttachments(cfg config.ComposeFile, serviceName string, service config.Service) []NetworkAttachment {
	if len(service.Networks) == 0 {
		return nil
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

	if service.Restart != "" && !ov.AutoRemove {
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
		target := envFile
		if !filepath.IsAbs(target) && cfg.ProjectDir != "" {
			target = filepath.Join(cfg.ProjectDir, target)
		}
		args = append(args, "--env-file", target)
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
	for _, entry := range ov.Env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			envMap[k] = v
		} else if entry != "" {
			args = append(args, "-e", entry)
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

	labels := make(map[string]string, len(service.Labels)+2)
	for k, v := range service.Labels {
		labels[k] = v
	}
	if proj := cfg.ProjectName(); proj != "" && cfg.Name != "" {
		labels[labelComposeProject] = proj
	}
	if serviceName != "" {
		labels[labelComposeService] = serviceName
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
	} else if len(service.Command) > 0 {
		trailingArgs = append(trailingArgs, service.Command...)
	}

	args = append(args, image)
	args = append(args, trailingArgs...)
	return args
}

func formatVolumeMount(cfg config.ComposeFile, mount config.VolumeMount) string {
	source := mount.Source
	if source != "" {
		if _, isNamed := cfg.Volumes[source]; isNamed {
			source = cfg.VolumeName(source)
		} else if strings.EqualFold(mount.Type, "bind") && !filepath.IsAbs(source) && cfg.ProjectDir != "" {
			source = filepath.Join(cfg.ProjectDir, source)
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
	switch strings.ToUpper(test[0]) {
	case "CMD-SHELL":
		if len(test) > 1 {
			return strings.Join(test[1:], " ")
		}
		return ""
	case "CMD":
		if len(test) > 1 {
			return strings.Join(test[1:], " ")
		}
		return ""
	default:
		return strings.Join(test, " ")
	}
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
