package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const envPassthroughSentinel = "\x00__WSLC_COMPOSE_ENV_LOOKUP__"

type LoadOptions struct {
	Files       []string
	ProjectName string
	EnvFile     string
	Profiles    []string
}

type ComposeFile struct {
	Name        string             `yaml:"name,omitempty"`
	Services    map[string]Service `yaml:"services"`
	Networks    map[string]Network `yaml:"networks,omitempty"`
	Volumes     map[string]Volume  `yaml:"volumes,omitempty"`
	AllServices map[string]Service `yaml:"-"`
	BaseDir     string             `yaml:"-"`
	ProjectDir  string             `yaml:"-"`
	FilePath    string             `yaml:"-"`
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
	ExtraHosts      ExtraHosts      `yaml:"extra_hosts,omitempty"`
	DNS             StringList      `yaml:"dns,omitempty"`
	Privileged      bool            `yaml:"privileged,omitempty"`
	Init            bool            `yaml:"init,omitempty"`
	ShmSize         string          `yaml:"shm_size,omitempty"`
	TTY             bool            `yaml:"tty,omitempty"`
	StdinOpen       bool            `yaml:"stdin_open,omitempty"`
	StopGracePeriod string          `yaml:"stop_grace_period,omitempty"`
	Ports           PortList        `yaml:"ports,omitempty"`
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

	paths, err := resolveComposeFiles(explicitPath, opt.Files, opt.EnvFile)
	if err != nil {
		return ComposeFile{}, err
	}

	absPath, err := filepath.Abs(paths[0])
	if err != nil {
		absPath = paths[0]
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

	var cfg ComposeFile
	for i, p := range paths {
		part, rootNode, err := loadSingleComposeFile(p, lookup)
		if err != nil {
			return ComposeFile{}, err
		}
		if i == 0 {
			cfg = part
		} else {
			cfg = mergeComposeFiles(cfg, part, rootNode)
		}
	}

	if len(cfg.Services) == 0 {
		return ComposeFile{}, fmt.Errorf("compose file %q defines no services", paths[0])
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

		if len(svc.Build.Args) > 0 {
			for k, v := range svc.Build.Args {
				if v == envPassthroughSentinel {
					if resolved, ok := lookup(k); ok {
						svc.Build.Args[k] = resolved
					} else {
						delete(svc.Build.Args, k)
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

	allServices := make(map[string]Service, len(cfg.Services))
	for name, svc := range cfg.Services {
		allServices[name] = svc
	}
	cfg.AllServices = allServices

	activeProfiles := collectActiveProfiles(opt.Profiles, lookup)
	if len(activeProfiles) > 0 {
		cfg = cfg.FilterByProfiles(activeProfiles, nil)
	}

	return cfg, nil
}

func loadSingleComposeFile(filePath string, lookup func(string) (string, bool)) (ComposeFile, *yaml.Node, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return ComposeFile{}, nil, fmt.Errorf("failed to read compose file %q: %w", filePath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return ComposeFile{}, nil, fmt.Errorf("failed to parse compose file %q: %w", filePath, err)
	}
	if doc.Kind == 0 {
		return ComposeFile{}, nil, nil
	}

	if err := interpolateYAMLNode(&doc, lookup, make(map[*yaml.Node]bool)); err != nil {
		return ComposeFile{}, nil, fmt.Errorf("failed to interpolate variables in %q: %w", filePath, err)
	}

	var cfg ComposeFile
	if err := doc.Decode(&cfg); err != nil {
		return ComposeFile{}, nil, fmt.Errorf("failed to parse compose file %q: %w", filePath, err)
	}

	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = derefAlias(doc.Content[0])
	}
	return cfg, root, nil
}

func interpolateYAMLNode(node *yaml.Node, lookup func(string) (string, bool), visited map[*yaml.Node]bool) error {
	if node == nil || visited[node] {
		return nil
	}
	visited[node] = true

	switch node.Kind {
	case yaml.DocumentNode, yaml.MappingNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := interpolateYAMLNode(child, lookup, visited); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		if node.Alias != nil {
			return interpolateYAMLNode(node.Alias, lookup, visited)
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil
		}
		if (node.Tag == "!!str" || node.Tag == "") && strings.Contains(node.Value, "$") {
			expanded, err := Interpolate(node.Value, lookup)
			if err != nil {
				return err
			}
			node.Value = expanded
			if node.Style == 0 {
				node.Tag = inferInterpolatedTag(expanded)
			} else {
				node.Tag = "!!str"
			}
		}
	}
	return nil
}

func inferInterpolatedTag(val string) string {
	switch val {
	case "true", "false", "True", "False", "TRUE", "FALSE":
		return "!!bool"
	}
	if isCanonicalInt(val) {
		if _, err := strconv.ParseInt(val, 10, 64); err == nil {
			return "!!int"
		}
	}
	return "!!str"
}

func isCanonicalInt(s string) bool {
	if s == "" {
		return false
	}
	start := 0
	if s[0] == '-' || s[0] == '+' {
		start = 1
	}
	if start >= len(s) {
		return false
	}
	if len(s)-start > 1 && s[start] == '0' {
		return false
	}
	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func collectActiveProfiles(optProfiles []string, lookup func(string) (string, bool)) []string {
	var out []string
	for _, p := range optProfiles {
		for _, part := range strings.Split(p, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	if envProfiles, ok := lookup("COMPOSE_PROFILES"); ok {
		for _, part := range strings.Split(envProfiles, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

func mergeComposeFiles(base, override ComposeFile, overrideRoot *yaml.Node) ComposeFile {
	out := base
	if hasMappingKey(overrideRoot, "name") || override.Name != "" {
		out.Name = override.Name
	}

	if len(override.Networks) > 0 {
		if out.Networks == nil {
			out.Networks = make(map[string]Network, len(override.Networks))
		}
		for name, ovNet := range override.Networks {
			if baseNet, exists := out.Networks[name]; exists {
				netNode := lookupMappingPath(overrideRoot, "networks", name)
				out.Networks[name] = mergeNetwork(baseNet, ovNet, netNode)
			} else {
				out.Networks[name] = ovNet
			}
		}
	}

	if len(override.Volumes) > 0 {
		if out.Volumes == nil {
			out.Volumes = make(map[string]Volume, len(override.Volumes))
		}
		for name, ovVol := range override.Volumes {
			if baseVol, exists := out.Volumes[name]; exists {
				volNode := lookupMappingPath(overrideRoot, "volumes", name)
				out.Volumes[name] = mergeVolume(baseVol, ovVol, volNode)
			} else {
				out.Volumes[name] = ovVol
			}
		}
	}

	if len(override.Services) > 0 {
		if out.Services == nil {
			out.Services = make(map[string]Service, len(override.Services))
		}
		for name, ovSvc := range override.Services {
			if baseSvc, exists := out.Services[name]; exists {
				svcNode := lookupMappingPath(overrideRoot, "services", name)
				out.Services[name] = mergeService(baseSvc, ovSvc, svcNode)
			} else {
				out.Services[name] = ovSvc
			}
		}
	}

	return out
}

func mergeNetwork(base, override Network, node *yaml.Node) Network {
	out := base
	if hasMappingKey(node, "name") {
		out.Name = override.Name
	}
	if hasMappingKey(node, "driver") {
		out.Driver = override.Driver
	}
	if hasMappingKey(node, "external") {
		out.External = override.External
	}
	if hasMappingKey(node, "internal") {
		out.Internal = override.Internal
	}
	if len(override.Labels) > 0 {
		out.Labels = LabelMap(mergeStringMap(map[string]string(out.Labels), map[string]string(override.Labels)))
	}
	if ipamNode, ok := lookupMappingValue(node, "ipam"); ok {
		if hasMappingKey(ipamNode, "driver") {
			out.IPAM.Driver = override.IPAM.Driver
		}
		if hasMappingKey(ipamNode, "config") {
			out.IPAM.Config = override.IPAM.Config
		}
	}
	return out
}

func mergeVolume(base, override Volume, node *yaml.Node) Volume {
	out := base
	if hasMappingKey(node, "driver") {
		out.Driver = override.Driver
	}
	if hasMappingKey(node, "name") {
		out.Name = override.Name
	}
	if hasMappingKey(node, "external") {
		out.External = override.External
	}
	if len(override.DriverOpts) > 0 {
		out.DriverOpts = mergeStringMap(out.DriverOpts, override.DriverOpts)
	}
	if len(override.Labels) > 0 {
		out.Labels = LabelMap(mergeStringMap(map[string]string(out.Labels), map[string]string(override.Labels)))
	}
	return out
}

func mergeService(base, override Service, node *yaml.Node) Service {
	out := base
	if hasMappingKey(node, "container_name") {
		out.ContainerName = override.ContainerName
	}
	if hasMappingKey(node, "image") {
		out.Image = override.Image
	}
	if buildNode, ok := lookupMappingValue(node, "build"); ok {
		buildNode = derefAlias(buildNode)
		if buildNode.Kind == yaml.ScalarNode {
			out.Build.Context = override.Build.Context
		} else {
			if hasMappingKey(buildNode, "context") {
				out.Build.Context = override.Build.Context
			}
			if hasMappingKey(buildNode, "dockerfile") {
				out.Build.Dockerfile = override.Build.Dockerfile
			}
			if hasMappingKey(buildNode, "target") {
				out.Build.Target = override.Build.Target
			}
			if len(override.Build.Args) > 0 {
				out.Build.Args = mergeStringMap(out.Build.Args, override.Build.Args)
			}
		}
	}
	if hasMappingKey(node, "command") {
		out.Command = override.Command
	}
	if hasMappingKey(node, "entrypoint") {
		out.Entrypoint = override.Entrypoint
	}
	if hasMappingKey(node, "working_dir") {
		out.WorkingDir = override.WorkingDir
	}
	if hasMappingKey(node, "user") {
		out.User = override.User
	}
	if hasMappingKey(node, "hostname") {
		out.Hostname = override.Hostname
	}
	if len(override.Labels) > 0 {
		out.Labels = LabelMap(mergeStringMap(map[string]string(out.Labels), map[string]string(override.Labels)))
	}
	if len(override.ExtraHosts) > 0 {
		out.ExtraHosts = mergeExtraHosts(out.ExtraHosts, override.ExtraHosts)
	}
	if len(override.DNS) > 0 {
		out.DNS = StringList(mergeUniqueStrings([]string(out.DNS), []string(override.DNS)))
	}
	if hasMappingKey(node, "privileged") {
		out.Privileged = override.Privileged
	}
	if hasMappingKey(node, "init") {
		out.Init = override.Init
	}
	if hasMappingKey(node, "shm_size") {
		out.ShmSize = override.ShmSize
	}
	if hasMappingKey(node, "tty") {
		out.TTY = override.TTY
	}
	if hasMappingKey(node, "stdin_open") {
		out.StdinOpen = override.StdinOpen
	}
	if hasMappingKey(node, "stop_grace_period") {
		out.StopGracePeriod = override.StopGracePeriod
	}
	if len(override.Ports) > 0 {
		out.Ports = PortList(mergeUniqueStrings([]string(out.Ports), []string(override.Ports)))
	}
	if len(override.Volumes) > 0 {
		out.Volumes = mergeVolumes(out.Volumes, override.Volumes)
	}
	if len(override.Networks) > 0 {
		if out.Networks == nil {
			out.Networks = make(ServiceNetworks, len(override.Networks))
		}
		for netName, ovCfg := range override.Networks {
			if baseCfg, exists := out.Networks[netName]; exists {
				merged := baseCfg
				if len(ovCfg.Aliases) > 0 {
					merged.Aliases = mergeUniqueStrings(merged.Aliases, ovCfg.Aliases)
				}
				if ovCfg.IPv4Address != "" {
					merged.IPv4Address = ovCfg.IPv4Address
				}
				out.Networks[netName] = merged
			} else {
				out.Networks[netName] = ovCfg
			}
		}
	}
	if len(override.EnvFile) > 0 {
		out.EnvFile = StringList(mergeUniqueStrings([]string(out.EnvFile), []string(override.EnvFile)))
	}
	if len(override.Environment) > 0 {
		out.Environment = EnvironmentMap(mergeStringMap(map[string]string(out.Environment), map[string]string(override.Environment)))
	}
	if hasMappingKey(node, "restart") {
		out.Restart = override.Restart
	}
	if hcNode, ok := lookupMappingValue(node, "healthcheck"); ok {
		out.HealthCheck = mergeHealthCheck(out.HealthCheck, override.HealthCheck, hcNode)
	}
	if len(override.DependsOn) > 0 {
		merged := make(DependsOnConfig, len(out.DependsOn)+len(override.DependsOn))
		for k, v := range out.DependsOn {
			merged[k] = v
		}
		for k, v := range override.DependsOn {
			merged[k] = v
		}
		out.DependsOn = merged
	}
	if len(override.Profiles) > 0 {
		out.Profiles = mergeUniqueStrings(out.Profiles, override.Profiles)
	}
	return out
}

func mergeHealthCheck(base, override *HealthCheck, node *yaml.Node) *HealthCheck {
	if override == nil {
		return nil
	}
	if base == nil {
		cp := *override
		return &cp
	}
	out := *base
	if hasMappingKey(node, "test") {
		out.Test = override.Test
	}
	if hasMappingKey(node, "interval") {
		out.Interval = override.Interval
	}
	if hasMappingKey(node, "timeout") {
		out.Timeout = override.Timeout
	}
	if hasMappingKey(node, "retries") {
		out.Retries = override.Retries
	}
	if hasMappingKey(node, "start_period") {
		out.StartPeriod = override.StartPeriod
	}
	if hasMappingKey(node, "disable") {
		out.Disable = override.Disable
	}
	return &out
}

func mergeStringMap(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func mergeUniqueStrings(base, override []string) []string {
	out := make([]string, 0, len(base)+len(override))
	seen := make(map[string]bool, len(base)+len(override))
	for _, s := range base {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range override {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func mergeVolumes(base, override VolumeMounts) VolumeMounts {
	out := make(VolumeMounts, len(base))
	copy(out, base)
	for _, ov := range override {
		replaced := false
		if ov.Target != "" {
			for i, existing := range out {
				if existing.Target == ov.Target {
					out[i] = ov
					replaced = true
					break
				}
			}
		}
		if !replaced {
			out = append(out, ov)
		}
	}
	return out
}

func mergeExtraHosts(base, override ExtraHosts) ExtraHosts {
	out := make(ExtraHosts, len(base))
	copy(out, base)
	for _, ov := range override {
		ovHost := extraHostKey(ov)
		replaced := false
		if ovHost != "" {
			for i, existing := range out {
				if extraHostKey(existing) == ovHost {
					out[i] = ov
					replaced = true
					break
				}
			}
		}
		if !replaced {
			out = append(out, ov)
		}
	}
	return out
}

func extraHostKey(entry string) string {
	if host, _, ok := strings.Cut(entry, ":"); ok {
		return host
	}
	if host, _, ok := strings.Cut(entry, "="); ok {
		return host
	}
	return entry
}

func lookupMappingValue(node *yaml.Node, key string) (*yaml.Node, bool) {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		if kNode.Kind == yaml.ScalarNode && kNode.Value == key {
			return derefAlias(node.Content[i+1]), true
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		if kNode.Tag == "!!merge" {
			vNode := derefAlias(node.Content[i+1])
			if vNode.Kind == yaml.MappingNode {
				if found, ok := lookupMappingValue(vNode, key); ok {
					return found, true
				}
			} else if vNode.Kind == yaml.SequenceNode {
				for _, item := range vNode.Content {
					if found, ok := lookupMappingValue(item, key); ok {
						return found, true
					}
				}
			}
		}
	}
	return nil, false
}

func lookupMappingPath(node *yaml.Node, keys ...string) *yaml.Node {
	cur := node
	for _, key := range keys {
		next, ok := lookupMappingValue(cur, key)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func hasMappingKey(node *yaml.Node, key string) bool {
	_, ok := lookupMappingValue(node, key)
	return ok
}

func (c ComposeFile) ComposeBaseDir() string {
	if c.BaseDir != "" {
		return c.BaseDir
	}
	return c.ProjectDir
}

func (c ComposeFile) ResolvePath(p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") {
			return path.Clean(p)
		}
		return p
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
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
	if len(out.AllServices) == 0 {
		out.AllServices = c.Services
	}
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

func splitComposeFileList(raw string) []string {
	sep := string(os.PathListSeparator)
	parts := strings.Split(raw, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func resolveComposeFiles(explicitPath string, optFiles []string, envFile string) ([]string, error) {
	var rawPaths []string
	explicitList := splitComposeFileList(explicitPath)
	optList := make([]string, 0, len(optFiles))
	for _, f := range optFiles {
		optList = append(optList, splitComposeFileList(f)...)
	}

	if len(explicitList) > 0 && len(optList) > 0 {
		if len(explicitList) == 1 && optList[0] == explicitList[0] {
			rawPaths = optList
		} else {
			rawPaths = append(explicitList, optList...)
		}
	} else if len(optList) > 0 {
		rawPaths = optList
	} else if len(explicitList) > 0 {
		rawPaths = explicitList
	}

	if len(rawPaths) == 0 {
		if envCompose := os.Getenv("COMPOSE_FILE"); envCompose != "" {
			rawPaths = splitComposeFileList(envCompose)
		} else {
			dotenvPath := ".env"
			if envFile != "" {
				dotenvPath = envFile
			}
			if dotenv, err := LoadEnvFile(dotenvPath, false); err == nil {
				if envCompose := dotenv["COMPOSE_FILE"]; envCompose != "" {
					rawPaths = splitComposeFileList(envCompose)
				}
			}
		}
	}

	if len(rawPaths) > 0 {
		for _, p := range rawPaths {
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("compose file %q not found: %w", p, err)
			}
		}
		return rawPaths, nil
	}

	candidates := []string{
		"compose.yaml",
		"compose.yml",
		"docker-compose.yaml",
		"docker-compose.yml",
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return []string{candidate}, nil
		}
	}

	return nil, fmt.Errorf("no compose file found (expected compose.yaml, compose.yml, docker-compose.yaml, or docker-compose.yml)")
}
