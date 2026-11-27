package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"wslc-compose/internal/config"
)

type CPOptions struct {
	Archive    bool
	FollowLink bool
}

type PortOptions struct {
	Protocol string
}

type KillOptions struct {
	Signal string
}

type StatsOptions struct {
	NoStream bool
}

type EventsOptions struct {
	JSON bool
}

var statsPollInterval = time.Second

func ExtractCPServices(src string, dst string) []string {
	var services []string
	if svc, _, ok := parseCPTarget(src); ok {
		services = append(services, svc)
	}
	if svc, _, ok := parseCPTarget(dst); ok {
		services = append(services, svc)
	}
	return services
}

func CP(ctx context.Context, cfg config.ComposeFile, src string, dst string, opts ...CPOptions) error {
	var opt CPOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	srcSvc, srcPath, srcIsSvc := parseCPTarget(src)
	dstSvc, dstPath, dstIsSvc := parseCPTarget(dst)

	if !srcIsSvc && !dstIsSvc {
		return fmt.Errorf("either source or destination must specify a service in SERVICE:PATH format")
	}
	if srcIsSvc && dstIsSvc {
		return fmt.Errorf("copying directly between two services is not supported")
	}

	resolvedSrc := src
	resolvedDst := dst

	if srcIsSvc {
		if _, ok := cfg.Services[srcSvc]; !ok {
			return fmt.Errorf("service %q not found in compose file", srcSvc)
		}
		resolvedSrc = cfg.ContainerName(srcSvc) + ":" + srcPath
	}
	if dstIsSvc {
		if _, ok := cfg.Services[dstSvc]; !ok {
			return fmt.Errorf("service %q not found in compose file", dstSvc)
		}
		resolvedDst = cfg.ContainerName(dstSvc) + ":" + dstPath
	}

	args := []string{"cp"}
	if opt.Archive {
		args = append(args, "-a")
	}
	if opt.FollowLink {
		args = append(args, "-L")
	}
	args = append(args, resolvedSrc, resolvedDst)

	if resolvedSrc == "-" || resolvedDst == "-" {
		return RunWSLCInteractive(ctx, args...)
	}

	if _, err := RunWSLC(ctx, args...); err != nil {
		return fmt.Errorf("failed to copy %q to %q: %w", src, dst, err)
	}
	return nil
}

func parseCPTarget(arg string) (string, string, bool) {
	if arg == "" || arg == "-" || isWindowsDrivePath(arg) {
		return "", "", false
	}
	svc, remotePath, ok := strings.Cut(arg, ":")
	if !ok || svc == "" || remotePath == "" {
		return "", "", false
	}
	if strings.ContainsAny(svc, "/\\") {
		return "", "", false
	}
	return svc, remotePath, true
}

func isWindowsDrivePath(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	ch := p[0]
	if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')) {
		return false
	}
	return len(p) == 2 || p[2] == '\\' || p[2] == '/'
}

func Port(ctx context.Context, cfg config.ComposeFile, service string, privatePort string, opts ...PortOptions) error {
	var opt PortOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if _, ok := cfg.Services[service]; !ok {
		return fmt.Errorf("service %q not found in compose file", service)
	}

	portStr := strings.TrimSpace(privatePort)
	proto := strings.ToLower(strings.TrimSpace(opt.Protocol))
	if basePort, embeddedProto, hasProto := strings.Cut(portStr, "/"); hasProto {
		portStr = strings.TrimSpace(basePort)
		if proto == "" {
			proto = strings.ToLower(strings.TrimSpace(embeddedProto))
		}
	}
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return fmt.Errorf("unsupported protocol %q: expected \"tcp\" or \"udp\"", proto)
	}

	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid private port %q: expected a port number between 1 and 65535", privatePort)
	}

	containerName := cfg.ContainerName(service)
	portKey := fmt.Sprintf("%d/%s", portNum, proto)

	inspectOut, inspectErr := RunWSLC(ctx, "inspect", containerName)
	if inspectErr == nil {
		if binding := extractPortFromInspectJSON(inspectOut, portKey); binding != "" {
			fmt.Fprintln(stdout(), binding)
			return nil
		}
	}

	containers, listErr := ListContainers(ctx)
	if listErr == nil {
		for _, c := range containers {
			if c.Name == containerName {
				if binding := extractPortFromPortsString(c.Ports, portKey); binding != "" {
					fmt.Fprintln(stdout(), binding)
					return nil
				}
				break
			}
		}
	}

	if inspectErr != nil && errors.Is(inspectErr, ErrNotFound) {
		return fmt.Errorf("container %q for service %q not found", containerName, service)
	}

	return fmt.Errorf("no public port %q published for service %q", portKey, service)
}

type inspectPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type inspectPortDocument struct {
	NetworkSettings struct {
		Ports map[string][]inspectPortBinding `json:"Ports"`
	} `json:"NetworkSettings"`
	HostConfig struct {
		PortBindings map[string][]inspectPortBinding `json:"PortBindings"`
	} `json:"HostConfig"`
}

func extractPortFromInspectJSON(raw string, portKey string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	var docs []inspectPortDocument
	if err := json.Unmarshal([]byte(trimmed), &docs); err != nil {
		var single inspectPortDocument
		if singleErr := json.Unmarshal([]byte(trimmed), &single); singleErr != nil {
			return ""
		}
		docs = []inspectPortDocument{single}
	}

	for _, doc := range docs {
		if binding := lookupPortBinding(doc.NetworkSettings.Ports, portKey); binding != "" {
			return binding
		}
		if binding := lookupPortBinding(doc.HostConfig.PortBindings, portKey); binding != "" {
			return binding
		}
	}

	return ""
}

func lookupPortBinding(ports map[string][]inspectPortBinding, portKey string) string {
	for k, bindings := range ports {
		if !strings.EqualFold(k, portKey) {
			continue
		}
		for _, b := range bindings {
			hostPort := strings.TrimSpace(b.HostPort)
			if hostPort == "" {
				continue
			}
			hostIP := strings.TrimSpace(b.HostIP)
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			if strings.Contains(hostIP, ":") && !strings.HasPrefix(hostIP, "[") {
				return fmt.Sprintf("[%s]:%s", hostIP, hostPort)
			}
			return fmt.Sprintf("%s:%s", hostIP, hostPort)
		}
	}
	return ""
}

func extractPortFromPortsString(portsRaw string, portKey string) string {
	for _, entry := range strings.Split(portsRaw, ",") {
		seg := strings.TrimSpace(entry)
		if seg == "" {
			continue
		}
		hostPart, containerPart, ok := strings.Cut(seg, "->")
		if !ok {
			continue
		}
		hostPart = strings.TrimSpace(hostPart)
		containerPart = strings.TrimSpace(containerPart)
		if !strings.EqualFold(containerPart, portKey) {
			continue
		}
		if strings.HasPrefix(hostPart, ":::") {
			return "[::]:" + strings.TrimPrefix(hostPart, ":::")
		}
		if !strings.Contains(hostPart, ":") {
			return "0.0.0.0:" + hostPart
		}
		return hostPart
	}
	return ""
}

func Kill(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...KillOptions) error {
	var opt KillOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	signalName := strings.TrimSpace(opt.Signal)
	if signalName == "" {
		signalName = "SIGKILL"
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	ui := newProgressUI("Killing")
	for _, name := range names {
		ui.add("svc:"+name, "Container", cfg.ContainerName(name))
	}
	ui.start()

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	for _, svcName := range names {
		name := svcName
		containerName := cfg.ContainerName(name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "svc:" + name
			ui.setRunning(id, "Killing")
			if killErr := KillContainer(ctx, containerName, signalName); killErr != nil {
				ui.setFailed(id, "Error")
				recordErr(killErr)
				return
			}
			ui.setDone(id, "Killed")
		}()
	}
	wg.Wait()

	ui.finish()
	return errors.Join(errs...)
}

func Stats(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...StatsOptions) error {
	var opt StatsOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}

	containers, err := ListContainers(ctx)
	if err != nil {
		return err
	}

	runningByName := make(map[string]bool, len(containers))
	for _, c := range containers {
		if c.Name != "" && parseStateFromStatusText(c.Status).Status == "running" {
			runningByName[c.Name] = true
		}
	}

	var runningTargets []string
	for _, name := range names {
		cName := cfg.ContainerName(name)
		if runningByName[cName] {
			runningTargets = append(runningTargets, cName)
		}
	}

	out := stdout()
	if len(runningTargets) == 0 {
		fmt.Fprintln(out, "No running containers")
		return nil
	}

	args := append([]string{"stats"}, runningTargets...)

	if opt.NoStream {
		res, runErr := RunWSLC(ctx, args...)
		if runErr != nil {
			return fmt.Errorf("failed to retrieve container stats: %w", runErr)
		}
		if res != "" {
			fmt.Fprintln(out, res)
		}
		return nil
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	renderSnapshot := func() error {
		res, runErr := RunWSLC(sigCtx, args...)
		if runErr != nil {
			if sigCtx.Err() != nil || errors.Is(runErr, context.Canceled) {
				return context.Canceled
			}
			return fmt.Errorf("failed to retrieve container stats: %w", runErr)
		}
		if res != "" {
			fmt.Fprintf(out, "\033[H\033[2J%s\n", res)
		} else {
			fmt.Fprint(out, "\033[H\033[2J")
		}
		return nil
	}

	if err := renderSnapshot(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	interval := statsPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCtx.Done():
			return nil
		case <-ticker.C:
			if err := renderSnapshot(); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
	}
}

type composeEventJSON struct {
	Timestamp string `json:"timestamp"`
	Container string `json:"container"`
	Service   string `json:"service"`
	Event     string `json:"event"`
}

func Events(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...EventsOptions) error {
	var opt EventsOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	containerToService := make(map[string]string, len(names))
	containerNames := make([]string, 0, len(names))
	for _, name := range names {
		cName := cfg.ContainerName(name)
		containerToService[cName] = name
		containerNames = append(containerNames, cName)
	}

	idToContainer := make(map[string]string)
	if containers, listErr := ListContainers(ctx); listErr == nil {
		for _, c := range containers {
			if _, ok := containerToService[c.Name]; !ok || c.ID == "" {
				continue
			}
			idToContainer[c.ID] = c.Name
			if len(c.ID) > 12 {
				idToContainer[c.ID[:12]] = c.Name
			}
		}
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	out := stdout()
	_, runErr := RunWSLCStream(sigCtx, func(line string) {
		svc, container, matched := matchEventLine(line, containerNames, containerToService, idToContainer)
		if !matched {
			return
		}
		if opt.JSON {
			fmt.Fprintln(out, formatEventJSON(line, container, svc))
			return
		}
		fmt.Fprintln(out, line)
	}, "events")

	if runErr != nil {
		if sigCtx.Err() != nil || errors.Is(runErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("failed to stream container events: %w", runErr)
	}
	return nil
}

func matchEventLine(line string, containerNames []string, containerToService map[string]string, idToContainer map[string]string) (string, string, bool) {
	for _, cName := range containerNames {
		if containsToken(line, cName, false) {
			return containerToService[cName], cName, true
		}
	}
	for id, cName := range idToContainer {
		if containsToken(line, id, true) {
			return containerToService[cName], cName, true
		}
	}
	return "", "", false
}

func containsToken(line string, token string, allowHexSuffix bool) bool {
	if token == "" {
		return false
	}
	searchFrom := 0
	for {
		idx := strings.Index(line[searchFrom:], token)
		if idx < 0 {
			return false
		}
		start := searchFrom + idx
		end := start + len(token)

		leftOK := start == 0 || !isIdentByte(line[start-1])
		rightOK := end == len(line) || !isIdentByte(line[end])
		if !rightOK && allowHexSuffix && isHexByte(line[end]) {
			rightOK = true
		}
		if leftOK && rightOK {
			return true
		}
		searchFrom = start + 1
	}
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_'
}

func isHexByte(b byte) bool {
	return (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') || (b >= '0' && b <= '9')
}

func formatEventJSON(line string, container string, service string) string {
	ts, ev := splitEventTimestamp(line)
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	payload := composeEventJSON{
		Timestamp: ts,
		Container: container,
		Service:   service,
		Event:     ev,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return line
	}
	return string(raw)
}

func splitEventTimestamp(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			ts := firstStringField(obj, "timestamp", "time", "Time")
			ev := firstStringField(obj, "event", "action", "Action", "status", "Status")
			if ev == "" {
				ev = trimmed
			}
			return ts, ev
		}
	}

	fields := strings.Fields(trimmed)
	if len(fields) >= 2 && looksLikeTimestamp(fields[0]) {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
		if len(fields) >= 3 && looksLikeTimeOfDay(fields[1]) {
			ts := fields[0] + " " + fields[1]
			rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[1]))
			return ts, rest
		}
		return fields[0], rest
	}

	return "", trimmed
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func looksLikeTimestamp(token string) bool {
	if len(token) < 10 {
		return false
	}
	for i := 0; i < 4; i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return token[4] == '-' && token[7] == '-'
}

func looksLikeTimeOfDay(token string) bool {
	if len(token) < 8 {
		return false
	}
	return token[2] == ':' && token[5] == ':'
}
