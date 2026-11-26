package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Driver   string   `yaml:"driver,omitempty"`
	External bool     `yaml:"external,omitempty"`
	Labels   LabelMap `yaml:"labels,omitempty"`
	Name     string   `yaml:"name,omitempty"`
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
	DNS             StringOrSlice   `yaml:"dns,omitempty"`
	Privileged      bool            `yaml:"privileged,omitempty"`
	Init            bool            `yaml:"init,omitempty"`
	ShmSize         string          `yaml:"shm_size,omitempty"`
	TTY             bool            `yaml:"tty,omitempty"`
	StdinOpen       bool            `yaml:"stdin_open,omitempty"`
	StopGracePeriod string          `yaml:"stop_grace_period,omitempty"`
	Ports           []string        `yaml:"ports,omitempty"`
	Volumes         VolumeMounts    `yaml:"volumes,omitempty"`
	Networks        ServiceNetworks `yaml:"networks,omitempty"`
	EnvFile         StringOrSlice   `yaml:"env_file,omitempty"`
	Environment     EnvironmentMap  `yaml:"environment,omitempty"`
	Restart         string          `yaml:"restart,omitempty"`
	HealthCheck     *HealthCheck    `yaml:"healthcheck,omitempty"`
	DependsOn       DependsOnConfig `yaml:"depends_on,omitempty"`
	Profiles        []string        `yaml:"profiles,omitempty"`
}

type BuildConfig struct {
	Context    string            `yaml:"context,omitempty"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
	Target     string            `yaml:"target,omitempty"`
}

func (b BuildConfig) IsZero() bool {
	return b.Context == "" && b.Dockerfile == "" && len(b.Args) == 0 && b.Target == ""
}

func (b *BuildConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		b.Context = value.Value
		return nil
	}

	type rawBuild BuildConfig
	return value.Decode((*rawBuild)(b))
}

type StringOrSlice []string

func (s *StringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Tag == "!!null" || value.Value == "" {
			*s = nil
			return nil
		}
		*s = splitShellWords(value.Value)
		return nil
	}

	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

type LabelMap map[string]string

func (l *LabelMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.SequenceNode {
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		m := make(LabelMap, len(items))
		for _, item := range items {
			k, v, ok := strings.Cut(item, "=")
			if ok {
				m[k] = v
			} else if item != "" {
				m[item] = ""
			}
		}
		*l = m
		return nil
	}

	var m map[string]string
	if err := value.Decode(&m); err != nil {
		return err
	}
	*l = LabelMap(m)
	return nil
}

type EnvironmentMap map[string]string

func (e *EnvironmentMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.SequenceNode {
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		m := make(EnvironmentMap, len(items))
		for _, item := range items {
			if k, v, ok := strings.Cut(item, "="); ok {
				m[k] = v
			} else if item != "" {
				m[item] = envPassthroughSentinel
			}
		}
		*e = m
		return nil
	}

	if value.Kind == yaml.MappingNode {
		m := make(EnvironmentMap, len(value.Content)/2)
		for i := 0; i+1 < len(value.Content); i += 2 {
			kNode := value.Content[i]
			vNode := value.Content[i+1]
			if vNode.Kind == yaml.ScalarNode && vNode.Tag == "!!null" {
				m[kNode.Value] = envPassthroughSentinel
				continue
			}
			var val string
			if err := vNode.Decode(&val); err != nil {
				return err
			}
			m[kNode.Value] = val
		}
		*e = m
		return nil
	}

	return fmt.Errorf("invalid environment section: expected map or list")
}

type VolumeMount struct {
	Type     string `yaml:"type,omitempty"`
	Source   string `yaml:"source,omitempty"`
	Target   string `yaml:"target,omitempty"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
}

func (v *VolumeMount) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*v = parseShortVolume(value.Value)
		return nil
	}

	type rawVolume VolumeMount
	var rv rawVolume
	if err := value.Decode(&rv); err != nil {
		return err
	}
	*v = VolumeMount(rv)
	return nil
}

type VolumeMounts []VolumeMount

func parseShortVolume(spec string) VolumeMount {
	parts := splitVolumeSpec(spec)
	switch len(parts) {
	case 1:
		return VolumeMount{Type: "volume", Target: parts[0]}
	case 2:
		return VolumeMount{Type: inferMountType(parts[0]), Source: parts[0], Target: parts[1]}
	default:
		ro := false
		for _, opt := range strings.Split(parts[2], ",") {
			if strings.TrimSpace(opt) == "ro" {
				ro = true
				break
			}
		}
		return VolumeMount{Type: inferMountType(parts[0]), Source: parts[0], Target: parts[1], ReadOnly: ro}
	}
}

func splitVolumeSpec(spec string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(spec); i++ {
		if spec[i] != ':' {
			continue
		}
		if i == 1 && start == 0 && ((spec[0] >= 'a' && spec[0] <= 'z') || (spec[0] >= 'A' && spec[0] <= 'Z')) {
			if i+1 < len(spec) && (spec[i+1] == '\\' || spec[i+1] == '/') {
				continue
			}
		}
		parts = append(parts, spec[start:i])
		start = i + 1
	}
	parts = append(parts, spec[start:])
	return parts
}

func inferMountType(source string) string {
	if source == "" {
		return "volume"
	}
	if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "\\") || strings.Contains(source, "/") || strings.Contains(source, "\\") {
		return "bind"
	}
	if len(source) >= 2 && source[1] == ':' {
		return "bind"
	}
	return "volume"
}

type ServiceNetworkConfig struct {
	Aliases     []string `yaml:"aliases,omitempty"`
	IPv4Address string   `yaml:"ipv4_address,omitempty"`
}

type ServiceNetworks map[string]ServiceNetworkConfig

func (n *ServiceNetworks) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.SequenceNode {
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		m := make(ServiceNetworks, len(list))
		for _, name := range list {
			if name != "" {
				m[name] = ServiceNetworkConfig{}
			}
		}
		*n = m
		return nil
	}

	if value.Kind == yaml.MappingNode {
		m := make(ServiceNetworks, len(value.Content)/2)
		for i := 0; i+1 < len(value.Content); i += 2 {
			kNode := value.Content[i]
			vNode := value.Content[i+1]
			var cfg ServiceNetworkConfig
			if vNode.Kind != yaml.ScalarNode || vNode.Tag != "!!null" {
				if err := vNode.Decode(&cfg); err != nil {
					return err
				}
			}
			m[kNode.Value] = cfg
		}
		*n = m
		return nil
	}

	return fmt.Errorf("invalid networks section: expected list or map")
}

func (n ServiceNetworks) SortedNames() []string {
	names := make([]string, 0, len(n))
	for name := range n {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type HealthCheckTest []string

func (t *HealthCheckTest) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Value == "" {
			*t = nil
			return nil
		}
		*t = HealthCheckTest{"CMD-SHELL", value.Value}
		return nil
	}

	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*t = HealthCheckTest(list)
	return nil
}

type HealthCheck struct {
	Test        HealthCheckTest `yaml:"test,omitempty"`
	Interval    string          `yaml:"interval,omitempty"`
	Timeout     string          `yaml:"timeout,omitempty"`
	Retries     int             `yaml:"retries,omitempty"`
	StartPeriod string          `yaml:"start_period,omitempty"`
	Disable     bool            `yaml:"disable,omitempty"`
}

type DependencyCondition struct {
	Condition string `yaml:"condition,omitempty"`
}

type DependsOnConfig map[string]DependencyCondition

func (d *DependsOnConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.SequenceNode {
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		m := make(DependsOnConfig, len(list))
		for _, dep := range list {
			if dep != "" {
				m[dep] = DependencyCondition{Condition: "service_started"}
			}
		}
		*d = m
		return nil
	}

	if value.Kind == yaml.MappingNode {
		m := make(DependsOnConfig, len(value.Content)/2)
		for i := 0; i+1 < len(value.Content); i += 2 {
			kNode := value.Content[i]
			vNode := value.Content[i+1]
			var cond DependencyCondition
			if vNode.Kind != yaml.ScalarNode || vNode.Tag != "!!null" {
				if err := vNode.Decode(&cond); err != nil {
					return err
				}
			}
			if cond.Condition == "" {
				cond.Condition = "service_started"
			}
			switch cond.Condition {
			case "service_started", "service_healthy", "service_completed_successfully":
			default:
				return fmt.Errorf("unsupported depends_on condition %q for service %q", cond.Condition, kNode.Value)
			}
			m[kNode.Value] = cond
		}
		*d = m
		return nil
	}

	return fmt.Errorf("invalid depends_on section: expected list or map")
}

func (d DependsOnConfig) SortedNames() []string {
	names := make([]string, 0, len(d))
	for name := range d {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	projectDir := filepath.Dir(absPath)

	envPath := filepath.Join(projectDir, ".env")
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
			cfg.Services[name] = svc
		}
	}

	cfg.ProjectDir = projectDir
	cfg.FilePath = absPath

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
		projectName = filepath.Base(projectDir)
	}
	cfg.Name = SanitizeProjectName(projectName)

	return cfg, nil
}

func (c ComposeFile) ProjectName() string {
	if c.Name != "" {
		return SanitizeProjectName(c.Name)
	}
	if c.ProjectDir != "" {
		return SanitizeProjectName(filepath.Base(c.ProjectDir))
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

func splitShellWords(input string) []string {
	var words []string
	var cur strings.Builder
	inToken := false
	var quote byte

	for i := 0; i < len(input); i++ {
		ch := input[i]
		if quote != 0 {
			if ch == '\\' && quote == '"' && i+1 < len(input) {
				i++
				cur.WriteByte(input[i])
				continue
			}
			if ch == quote {
				quote = 0
				continue
			}
			cur.WriteByte(ch)
			continue
		}

		switch ch {
		case ' ', '\t', '\n', '\r':
			if inToken {
				words = append(words, cur.String())
				cur.Reset()
				inToken = false
			}
		case '\'', '"':
			quote = ch
			inToken = true
		case '\\':
			inToken = true
			if i+1 < len(input) {
				i++
				cur.WriteByte(input[i])
			}
		default:
			cur.WriteByte(ch)
			inToken = true
		}
	}

	if inToken {
		words = append(words, cur.String())
	}
	return words
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
