package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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

type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Tag == "!!null" || value.Value == "" {
			*s = nil
			return nil
		}
		*s = StringList{value.Value}
		return nil
	}

	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = StringList(list)
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
