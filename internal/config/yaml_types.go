package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func derefAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

type mappingPair struct {
	key *yaml.Node
	val *yaml.Node
}

func expandMappingPairs(node *yaml.Node, visited map[*yaml.Node]bool) ([]mappingPair, error) {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node")
	}
	if visited == nil {
		visited = make(map[*yaml.Node]bool)
	}
	if visited[node] {
		return nil, nil
	}
	visited[node] = true
	defer delete(visited, node)

	var explicit []mappingPair
	explicitKeys := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		vNode := derefAlias(node.Content[i+1])
		if kNode.Tag == "!!merge" {
			continue
		}
		explicit = append(explicit, mappingPair{key: kNode, val: vNode})
		explicitKeys[kNode.Value] = true
	}

	var result []mappingPair
	mergedSeen := make(map[string]bool)
	appendMerged := func(subNode *yaml.Node) error {
		subPairs, err := expandMappingPairs(subNode, visited)
		if err != nil {
			return err
		}
		for _, p := range subPairs {
			if explicitKeys[p.key.Value] || mergedSeen[p.key.Value] {
				continue
			}
			mergedSeen[p.key.Value] = true
			result = append(result, p)
		}
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		if kNode.Tag != "!!merge" {
			continue
		}
		vNode := derefAlias(node.Content[i+1])
		switch vNode.Kind {
		case yaml.MappingNode:
			if err := appendMerged(vNode); err != nil {
				return nil, err
			}
		case yaml.SequenceNode:
			for _, elem := range vNode.Content {
				elem = derefAlias(elem)
				if elem.Kind == yaml.ScalarNode && elem.Tag == "!!null" {
					continue
				}
				if elem.Kind != yaml.MappingNode {
					return nil, fmt.Errorf("invalid yaml merge element: expected mapping")
				}
				if err := appendMerged(elem); err != nil {
					return nil, err
				}
			}
		case yaml.ScalarNode:
			if vNode.Tag != "!!null" {
				return nil, fmt.Errorf("invalid yaml merge value: expected mapping or sequence of mappings")
			}
		default:
			return nil, fmt.Errorf("invalid yaml merge value: expected mapping or sequence of mappings")
		}
	}

	if len(explicit) > 0 {
		lastIdx := make(map[string]int, len(explicit))
		for idx, p := range explicit {
			lastIdx[p.key.Value] = idx
		}
		for idx, p := range explicit {
			if lastIdx[p.key.Value] == idx {
				result = append(result, p)
			}
		}
	}

	return result, nil
}

type BuildArgs map[string]string

func (a *BuildArgs) UnmarshalYAML(value *yaml.Node) error {
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*a = nil
		return nil
	}
	if value.Kind == yaml.SequenceNode {
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		m := make(BuildArgs, len(items))
		for _, item := range items {
			if k, v, ok := strings.Cut(item, "="); ok {
				m[k] = v
			} else if item != "" {
				m[item] = envPassthroughSentinel
			}
		}
		*a = m
		return nil
	}
	if value.Kind == yaml.MappingNode {
		pairs, err := expandMappingPairs(value, nil)
		if err != nil {
			return err
		}
		m := make(BuildArgs, len(pairs))
		for _, p := range pairs {
			kNode := p.key
			vNode := p.val
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
		*a = m
		return nil
	}
	return fmt.Errorf("invalid build.args section: expected map or list")
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
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode {
		b.Context = value.Value
		return nil
	}

	var raw struct {
		Context    string    `yaml:"context,omitempty"`
		Dockerfile string    `yaml:"dockerfile,omitempty"`
		Args       BuildArgs `yaml:"args,omitempty"`
		Target     string    `yaml:"target,omitempty"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	b.Context = raw.Context
	b.Dockerfile = raw.Dockerfile
	b.Args = map[string]string(raw.Args)
	b.Target = raw.Target
	return nil
}

type PortList []string

func (p *PortList) UnmarshalYAML(value *yaml.Node) error {
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*p = nil
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("invalid ports section: expected list")
	}

	ports := make(PortList, 0, len(value.Content))
	for _, item := range value.Content {
		item = derefAlias(item)
		switch item.Kind {
		case yaml.ScalarNode:
			if item.Tag == "!!null" {
				continue
			}
			val := strings.TrimSpace(item.Value)
			if val != "" {
				ports = append(ports, val)
			}
		case yaml.MappingNode:
			var long struct {
				Target    string `yaml:"target"`
				Published string `yaml:"published"`
				HostIP    string `yaml:"host_ip"`
				Protocol  string `yaml:"protocol"`
				Mode      string `yaml:"mode"`
			}
			if err := item.Decode(&long); err != nil {
				return err
			}
			formatted, err := formatLongPort(long.HostIP, long.Published, long.Target, long.Protocol)
			if err != nil {
				return err
			}
			ports = append(ports, formatted)
		default:
			return fmt.Errorf("invalid port entry: expected string or mapping")
		}
	}
	*p = ports
	return nil
}

func formatLongPort(hostIP, published, target, protocol string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("invalid port entry: target port is required")
	}
	hostIP = strings.TrimSpace(hostIP)
	published = strings.TrimSpace(published)
	protocol = strings.ToLower(strings.TrimSpace(protocol))

	var b strings.Builder
	if hostIP != "" {
		if strings.Contains(hostIP, ":") && !strings.HasPrefix(hostIP, "[") {
			b.WriteByte('[')
			b.WriteString(hostIP)
			b.WriteByte(']')
		} else {
			b.WriteString(hostIP)
		}
		b.WriteByte(':')
		b.WriteString(published)
		b.WriteByte(':')
		b.WriteString(target)
	} else if published != "" {
		b.WriteString(published)
		b.WriteByte(':')
		b.WriteString(target)
	} else {
		b.WriteString(target)
	}

	if protocol != "" {
		b.WriteByte('/')
		b.WriteString(protocol)
	}
	return b.String(), nil
}

type ExtraHosts []string

func (e *ExtraHosts) UnmarshalYAML(value *yaml.Node) error {
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*e = nil
		return nil
	}
	if value.Kind == yaml.SequenceNode {
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*e = ExtraHosts(list)
		return nil
	}
	if value.Kind == yaml.MappingNode {
		pairs, err := expandMappingPairs(value, nil)
		if err != nil {
			return err
		}
		hosts := make(ExtraHosts, 0, len(pairs))
		for _, p := range pairs {
			kNode := p.key
			vNode := p.val
			host := strings.TrimSpace(kNode.Value)
			if host == "" {
				continue
			}
			if vNode.Kind == yaml.SequenceNode {
				var ips []string
				if err := vNode.Decode(&ips); err != nil {
					return err
				}
				for _, ip := range ips {
					ip = strings.TrimSpace(ip)
					if ip != "" {
						hosts = append(hosts, fmt.Sprintf("%s:%s", host, ip))
					}
				}
				continue
			}
			var ip string
			if err := vNode.Decode(&ip); err != nil {
				return err
			}
			ip = strings.TrimSpace(ip)
			if ip != "" {
				hosts = append(hosts, fmt.Sprintf("%s:%s", host, ip))
			}
		}
		*e = hosts
		return nil
	}
	return fmt.Errorf("invalid extra_hosts section: expected list or map")
}

type StringOrSlice []string

func (s *StringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	value = derefAlias(value)
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
	value = derefAlias(value)
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
	value = derefAlias(value)
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
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*e = nil
		return nil
	}
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
		pairs, err := expandMappingPairs(value, nil)
		if err != nil {
			return err
		}
		m := make(EnvironmentMap, len(pairs))
		for _, p := range pairs {
			kNode := p.key
			vNode := p.val
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
	value = derefAlias(value)
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

func isWindowsDriveVolumePrefix(spec string) bool {
	if len(spec) < 3 || spec[1] != ':' {
		return false
	}
	ch := spec[0]
	if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')) {
		return false
	}
	if spec[2] == '\\' {
		return true
	}
	if spec[2] != '/' {
		return false
	}
	nextColon := strings.IndexByte(spec[2:], ':')
	if nextColon < 0 {
		return false
	}
	after := spec[2+nextColon+1:]
	return len(after) > 0 && (after[0] == '/' || after[0] == '\\')
}

func splitVolumeSpec(spec string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(spec); i++ {
		if spec[i] != ':' {
			continue
		}
		if i == 1 && start == 0 && isWindowsDriveVolumePrefix(spec) {
			continue
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
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*n = nil
		return nil
	}
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
		pairs, err := expandMappingPairs(value, nil)
		if err != nil {
			return err
		}
		m := make(ServiceNetworks, len(pairs))
		for _, p := range pairs {
			kNode := p.key
			vNode := p.val
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
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode {
		if value.Tag == "!!null" || value.Value == "" {
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
	value = derefAlias(value)
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		*d = nil
		return nil
	}
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
		pairs, err := expandMappingPairs(value, nil)
		if err != nil {
			return err
		}
		m := make(DependsOnConfig, len(pairs))
		for _, p := range pairs {
			kNode := p.key
			vNode := p.val
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
				next := input[i+1]
				if next == '"' || next == '\\' || next == '$' || next == '`' {
					i++
					cur.WriteByte(next)
					continue
				}
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
