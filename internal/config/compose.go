package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const envPassthroughSentinel = "\x00__WSLC_COMPOSE_ENV_LOOKUP__"

type LoadOptions struct {
	ProjectName string
	EnvFile     string
	Profiles    []string
}

type ComposeFile struct {
	Name       string             `yaml:"name,omitempty"`
	Services   map[string]Service `yaml:"services"`
	Networks   map[string]Network `yaml:"networks,omitempty"`
	Volumes    map[string]Volume  `yaml:"volumes,omitempty"`
	BaseDir    string             `yaml:"-"`
	ProjectDir string             `yaml:"-"`
	FilePath   string             `yaml:"-"`
}

type IPAMConfig struct {
	Subnet  string `yaml:"subnet,omitempty"`
	Gateway string `yaml:"gateway,omitempty"`
}

type IPAM struct {
	Driver string       `yaml:"driver,omitempty"`
	Config []IPAMConfig `yaml:"config,omitempty"`
}

type Network struct {
	Name     string   `yaml:"name,omitempty"`
	Driver   string   `yaml:"driver,omitempty"`
	External bool     `yaml:"external,omitempty"`
	Internal bool     `yaml:"internal,omitempty"`
	Labels   LabelMap `yaml:"labels,omitempty"`
	IPAM     IPAM     `yaml:"ipam,omitempty"`
}

type Volume struct {
	Driver     string            `yaml:"driver,omitempty"`
	DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
	External   bool              `yaml:"external,omitempty"`
	Labels     LabelMap          `yaml:"labels,omitempty"`
	Name       string            `yaml:"name,omitempty"`
}

type Service struct {
	ContainerName   string          `yaml:"container_name,omitempty"`
	Image           string          `yaml:"image,omitempty"`
	Build           BuildConfig     `yaml:"build,omitempty"`
	Command         StringOrSlice   `yaml:"command,omitempty"`
	Entrypoint      StringOrSlice   `yaml:"entrypoint,omitempty"`
	WorkingDir      string          `yaml:"working_dir,omitempty"`
	User            string          `yaml:"user,omitempty"`
	Hostname        string          `yaml:"hostname,omitempty"`
	Labels          LabelMap        `yaml:"labels,omitempty"`
	ExtraHosts      []string        `yaml:"extra_hosts,omitempty"`
	DNS             StringList      `yaml:"dns,omitempty"`
	Privileged      bool            `yaml:"privileged,omitempty"`
	Init            bool            `yaml:"init,omitempty"`
	ShmSize         string          `yaml:"shm_size,omitempty"`
	TTY             bool            `yaml:"tty,omitempty"`
	StdinOpen       bool            `yaml:"stdin_open,omitempty"`
	StopGracePeriod string          `yaml:"stop_grace_period,omitempty"`
	Ports           []string        `yaml:"ports,omitempty"`
	Volumes         VolumeMounts    `yaml:"volumes,omitempty"`
	Networks        ServiceNetworks `yaml:"networks,omitempty"`
	EnvFile         StringList      `yaml:"env_file,omitempty"`
	Environment     EnvironmentMap  `yaml:"environment,omitempty"`
	Restart         string          `yaml:"restart,omitempty"`
	HealthCheck     *HealthCheck    `yaml:"healthcheck,omitempty"`
	DependsOn       DependsOnConfig `yaml:"depends_on,omitempty"`
	Profiles        []string        `yaml:"profiles,omitempty"`
}

func Load(explicitPath string, opts ...LoadOptions) (ComposeFile, error) {
	var opt LoadOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	path, err := resolveComposeFile(explicitPath)
	if err != nil {
		return ComposeFile{}, err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	baseDir := filepath.Dir(absPath)

	envPath := filepath.Join(baseDir, ".env")
	envRequired := false
	if opt.EnvFile != "" {
		envPath = opt.EnvFile
		envRequired = true
	}

	dotenv, err := LoadEnvFile(envPath, envRequired)
	if err != nil {
		return ComposeFile{}, err
	}
	lookup := buildEnvLookup(dotenv)

	raw, err := os.ReadFile(path)
	if err != nil {
		return ComposeFile{}, fmt.Errorf("failed to read compose file %q: %w", path, err)
	}

	expanded, err := Interpolate(string(raw), lookup)
	if err != nil {
		return ComposeFile{}, fmt.Errorf("failed to interpolate variables in %q: %w", path, err)
	}

	var cfg ComposeFile
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return ComposeFile{}, fmt.Errorf("failed to parse compose file %q: %w", path, err)
	}

	if len(cfg.Services) == 0 {
		return ComposeFile{}, fmt.Errorf("compose file %q defines no services", path)
	}

	cfg.BaseDir = baseDir
	cfg.ProjectDir = baseDir
	cfg.FilePath = absPath

	for name, svc := range cfg.Services {
		if len(svc.Environment) > 0 {
			for k, v := range svc.Environment {
				if v == envPassthroughSentinel {
					if resolved, ok := lookup(k); ok {
						svc.Environment[k] = resolved
					} else {
						delete(svc.Environment, k)
					}
				}
			}
		}

		if len(svc.EnvFile) > 0 {
			resolvedEnvFiles := make(StringList, 0, len(svc.EnvFile))
			for _, ef := range svc.EnvFile {
				if ef == "" {
					continue
				}
				resolvedEnvFiles = append(resolvedEnvFiles, cfg.ResolvePath(ef))
			}
			svc.EnvFile = resolvedEnvFiles
		}

		if len(svc.Volumes) > 0 {
			resolvedMounts := make(VolumeMounts, len(svc.Volumes))
			copy(resolvedMounts, svc.Volumes)
			for i, mount := range resolvedMounts {
				if mount.Source == "" {
					continue
				}
				if _, isNamed := cfg.Volumes[mount.Source]; isNamed && !strings.EqualFold(mount.Type, "bind") {
					continue
				}
				if strings.EqualFold(mount.Type, "bind") {
					resolvedMounts[i].Source = cfg.ResolvePath(mount.Source)
				}
			}
			svc.Volumes = resolvedMounts
		}

		cfg.Services[name] = svc
	}

	projectName := opt.ProjectName
	if projectName == "" {
		if envProj, ok := lookup("COMPOSE_PROJECT_NAME"); ok && envProj != "" {
			projectName = envProj
		}
	}
	if projectName == "" {
		projectName = cfg.Name
	}
	if projectName == "" {
		projectName = filepath.Base(baseDir)
	}
	cfg.Name = SanitizeProjectName(projectName)

	return cfg, nil
}

func (c ComposeFile) ComposeBaseDir() string {
	if c.BaseDir != "" {
		return c.BaseDir
	}
	return c.ProjectDir
}

func (c ComposeFile) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	base := c.ComposeBaseDir()
	if base == "" {
		return p
	}
	return filepath.Join(base, p)
}

func (c ComposeFile) ProjectName() string {
	if c.Name != "" {
		return SanitizeProjectName(c.Name)
	}
	if base := c.ComposeBaseDir(); base != "" {
		return SanitizeProjectName(filepath.Base(base))
	}
	return "default"
}

func (c ComposeFile) ContainerName(serviceName string) string {
	if svc, ok := c.Services[serviceName]; ok && svc.ContainerName != "" {
		return svc.ContainerName
	}
	return fmt.Sprintf("%s-%s-1", c.ProjectName(), serviceName)
}

func (c ComposeFile) NetworkName(networkKey string) string {
	if net, ok := c.Networks[networkKey]; ok {
		if net.Name != "" {
			return net.Name
		}
		if net.External {
			return networkKey
		}
		return fmt.Sprintf("%s_%s", c.ProjectName(), networkKey)
	}
	return networkKey
}

func (c ComposeFile) VolumeName(volumeKey string) string {
	if vol, ok := c.Volumes[volumeKey]; ok {
		if vol.Name != "" {
			return vol.Name
		}
		if vol.External {
			return volumeKey
		}
		return fmt.Sprintf("%s_%s", c.ProjectName(), volumeKey)
	}
	return volumeKey
}

func (c ComposeFile) FilterByProfiles(activeProfiles []string, explicitServices []string) ComposeFile {
	if len(c.Services) == 0 {
		return c
	}

	profileSet := make(map[string]bool, len(activeProfiles))
	wildcard := false
	for _, p := range activeProfiles {
		for _, part := range strings.Split(p, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if trimmed == "*" {
				wildcard = true
			}
			profileSet[trimmed] = true
		}
	}

	explicitSet := make(map[string]bool, len(explicitServices))
	for _, s := range explicitServices {
		explicitSet[s] = true
	}

	included := make(map[string]bool, len(c.Services))
	for name, svc := range c.Services {
		if wildcard || len(svc.Profiles) == 0 || explicitSet[name] {
			included[name] = true
			continue
		}
		for _, p := range svc.Profiles {
			if profileSet[p] {
				included[name] = true
				break
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for name := range included {
			svc, ok := c.Services[name]
			if !ok {
				continue
			}
			for dep := range svc.DependsOn {
				if _, exists := c.Services[dep]; exists && !included[dep] {
					included[dep] = true
					changed = true
				}
			}
		}
	}

	filtered := make(map[string]Service, len(included))
	for name := range included {
		filtered[name] = c.Services[name]
	}

	out := c
	out.Services = filtered
	return out
}

func SanitizeProjectName(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(lower))
	for i := 0; i < len(lower); i++ {
		ch := lower[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			b.WriteByte(ch)
		}
	}
	cleaned := strings.TrimLeft(b.String(), "-_")
	if cleaned == "" {
		return "default"
	}
	return cleaned
}

func ParseDurationSeconds(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(trimmed); err == nil && secs >= 0 {
		return secs, true
	}
	if d, err := time.ParseDuration(trimmed); err == nil && d >= 0 {
		return int(d.Seconds()), true
	}
	return 0, false
}

func resolveComposeFile(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("compose file %q not found: %w", explicitPath, err)
		}
		return explicitPath, nil
	}

	candidates := []string{
		"compose.yaml",
		"compose.yml",
		"docker-compose.yaml",
		"docker-compose.yml",
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no compose file found (expected compose.yaml, compose.yml, docker-compose.yaml, or docker-compose.yml)")
}
