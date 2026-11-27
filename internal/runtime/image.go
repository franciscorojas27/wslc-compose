package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"wslc-compose/internal/config"
)

type BuildOptions struct {
	NoCache    bool
	Pull       bool
	BuildArgs  []string
	ProjectDir string
}

type PullProgress struct {
	Percent int
	HasPct  bool
	Detail  string
	Layers  []int
}

type ImageInfo struct {
	Repository string
	Tag        string
	ID         string
	Size       string
}

var (
	percentRe    = regexp.MustCompile(`(\d{1,3})(?:\.\d+)?%`)
	sizeRatioRe  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([kmgt]?i?b)\s*/\s*(\d+(?:\.\d+)?)\s*([kmgt]?i?b)`)
	layerRatioRe = regexp.MustCompile(`\b(\d+)\s*/\s*(\d+)\b`)
	layerIDRe    = regexp.MustCompile(`^(?:sha256:)?([0-9a-fA-F]{6,64}):\s*(.+)$`)
	asciiBarRe   = regexp.MustCompile(`\[[=\->#\.\s]+\]`)
	multiSpaceRe = regexp.MustCompile(`\s{2,}`)
)

func buildImageArgs(tag string, build config.BuildConfig, opt BuildOptions) []string {
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

	if opt.Pull {
		args = append(args, "--pull")
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

	mergedArgs := make(map[string]string, len(build.Args)+len(opt.BuildArgs))
	for k, v := range build.Args {
		mergedArgs[k] = v
	}

	var bareArgs []string
	for _, entry := range opt.BuildArgs {
		if entry == "" {
			continue
		}
		if k, v, ok := strings.Cut(entry, "="); ok {
			if k != "" {
				mergedArgs[k] = v
			}
			continue
		}
		if val, ok := os.LookupEnv(entry); ok {
			mergedArgs[entry] = val
		} else {
			bareArgs = append(bareArgs, entry)
		}
	}

	if len(mergedArgs) > 0 {
		keys := make([]string, 0, len(mergedArgs))
		for k := range mergedArgs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--build-arg", k+"="+mergedArgs[k])
		}
	}

	if len(bareArgs) > 0 {
		sort.Strings(bareArgs)
		seen := make(map[string]bool, len(bareArgs))
		for _, k := range bareArgs {
			if seen[k] {
				continue
			}
			seen[k] = true
			if _, exists := mergedArgs[k]; !exists {
				args = append(args, "--build-arg", k)
			}
		}
	}

	args = append(args, buildContext)
	return args
}

func BuildImage(ctx context.Context, tag string, build config.BuildConfig, opts ...BuildOptions) error {
	var opt BuildOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	args := buildImageArgs(tag, build, opt)
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
