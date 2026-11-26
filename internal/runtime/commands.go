package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"wslc-compose/internal/config"

	"gopkg.in/yaml.v3"
)

type PSOptions struct {
	Quiet    bool
	All      bool
	Services bool
	Status   []string
	Format   string
}

type LogsOptions struct {
	Follow     bool
	Tail       string
	Timestamps bool
}

type ExecOptions struct {
	NoTTY   bool
	User    string
	Workdir string
	Env     []string
}

type RunOptions struct {
	Remove        bool
	Env           []string
	Workdir       string
	User          string
	Entrypoint    string
	HasEntrypoint bool
	NoDeps        bool
	Detach        bool
}

type ConfigOptions struct {
	Services bool
	Volumes  bool
	Quiet    bool
}

type RmOptions struct {
	Force   bool
	Stop    bool
	Volumes bool
}

type ImagesOptions struct {
	Quiet bool
}

type psJSONEntry struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
}

func PS(ctx context.Context, cfg config.ComposeFile, opts ...PSOptions) error {
	var opt PSOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	format := strings.ToLower(strings.TrimSpace(opt.Format))
	if format == "" {
		format = "table"
	}
	if format != "table" && format != "json" {
		return fmt.Errorf("unsupported format %q: expected \"table\" or \"json\"", opt.Format)
	}

	containers, err := ListContainers(ctx)
	if err != nil {
		return err
	}

	byName := make(map[string]ContainerInfo, len(containers))
	for _, c := range containers {
		if c.Name != "" {
			byName[c.Name] = c
		}
	}

	statusFilter := make(map[string]bool, len(opt.Status))
	for _, s := range opt.Status {
		trimmed := strings.ToLower(strings.TrimSpace(s))
		if trimmed != "" {
			statusFilter[trimmed] = true
		}
	}

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	type matchedEntry struct {
		service   string
		container ContainerInfo
		state     string
	}
	var matched []matchedEntry

	for _, svcName := range names {
		c, ok := byName[cfg.ContainerName(svcName)]
		if !ok {
			continue
		}
		parsed := parseStateFromStatusText(c.Status)
		if len(statusFilter) > 0 {
			if !statusFilter[parsed.Status] {
				continue
			}
		} else if !opt.All && parsed.Status != "running" {
			continue
		}
		matched = append(matched, matchedEntry{
			service:   svcName,
			container: c,
			state:     parsed.Status,
		})
	}

	if opt.Services {
		for _, item := range matched {
			fmt.Fprintln(os.Stdout, item.service)
		}
		return nil
	}

	if opt.Quiet {
		for _, item := range matched {
			if item.container.Name != "" {
				fmt.Fprintln(os.Stdout, item.container.Name)
			} else if item.container.ID != "" {
				fmt.Fprintln(os.Stdout, item.container.ID)
			}
		}
		return nil
	}

	if format == "json" {
		entries := make([]psJSONEntry, 0, len(matched))
		for _, item := range matched {
			entries = append(entries, psJSONEntry{
				Name:    item.container.Name,
				Service: item.service,
				Image:   item.container.Image,
				State:   item.state,
				Status:  item.container.Status,
				Ports:   item.container.Ports,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	tty := isStdoutTTY()
	rows := make([][]string, 0, len(matched))
	for _, item := range matched {
		status := item.container.Status
		if tty {
			switch item.state {
			case "running":
				status = ansiGreen + status + ansiReset
			case "exited", "dead":
				status = ansiRed + status + ansiReset
			default:
				if status != "" {
					status = ansiDim + status + ansiReset
				}
			}
		}
		rows = append(rows, []string{item.container.Name, item.service, status, item.container.Ports})
	}

	printTable(os.Stdout, []string{"NAME", "SERVICE", "STATUS", "PORTS"}, rows)
	return nil
}

func Volumes(ctx context.Context, cfg config.ComposeFile) error {
	if len(cfg.Volumes) == 0 {
		fmt.Fprintln(os.Stdout, "No volumes declared in compose file.")
		return nil
	}

	names := make([]string, 0, len(cfg.Volumes))
	for name := range cfg.Volumes {
		names = append(names, name)
	}
	sort.Strings(names)

	existing, listErr := ListVolumes(ctx)
	tty := isStdoutTTY()
	rows := make([][]string, 0, len(names))

	for _, key := range names {
		vol := cfg.Volumes[key]
		realName := cfg.VolumeName(key)
		driver := vol.Driver
		if driver == "" {
			driver = "local"
		}

		exists := false
		if listErr == nil {
			exists = existing[realName]
		} else {
			exists = VolumeExists(ctx, realName, vol)
		}

		existsStr := "no"
		if exists {
			existsStr = "yes"
			if tty {
				existsStr = ansiGreen + existsStr + ansiReset
			}
		} else if tty {
			existsStr = ansiDim + existsStr + ansiReset
		}

		rows = append(rows, []string{
			key,
			realName,
			driver,
			strconv.FormatBool(vol.External),
			existsStr,
		})
	}

	printTable(os.Stdout, []string{"VOLUME", "NAME", "DRIVER", "EXTERNAL", "EXISTS"}, rows)
	return nil
}

func Logs(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...LogsOptions) error {
	var opt LogsOptions
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

	maxNameLen := 0
	for _, name := range names {
		if len(name) > maxNameLen {
			maxNameLen = len(name)
		}
	}

	tty := isStdoutTTY()
	var (
		printMu sync.Mutex
		errMu   sync.Mutex
		errs    []error
		wg      sync.WaitGroup
	)

	for idx, svcName := range names {
		name := svcName
		containerName := cfg.ContainerName(name)
		color := logColors[idx%len(logColors)]
		padded := fmt.Sprintf("%-*s", maxNameLen, name)
		prefix := padded + " | "
		if tty {
			prefix = color + padded + ansiReset + " " + ansiDim + "|" + ansiReset + " "
		}

		args := []string{"logs"}
		if opt.Follow {
			args = append(args, "-f")
		}
		if opt.Timestamps {
			args = append(args, "-t")
		}
		if opt.Tail != "" && !strings.EqualFold(opt.Tail, "all") {
			args = append(args, "--tail", opt.Tail)
		}
		args = append(args, containerName)

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, runErr := RunWSLCStream(ctx, func(line string) {
				printMu.Lock()
				fmt.Fprintf(os.Stdout, "%s%s\n", prefix, line)
				printMu.Unlock()
			}, args...)
			if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, ErrNotFound) {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("failed to read logs for service %q: %w", name, runErr))
				errMu.Unlock()
			}
		}()
	}

	wg.Wait()
	return errors.Join(errs...)
}

func Exec(ctx context.Context, cfg config.ComposeFile, service string, command []string, opts ...ExecOptions) error {
	var opt ExecOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if _, ok := cfg.Services[service]; !ok {
		return fmt.Errorf("service %q not found in compose file", service)
	}
	if len(command) == 0 {
		return fmt.Errorf("command is required")
	}

	args := []string{"exec"}
	if !opt.NoTTY && isStdinTTY() {
		args = append(args, "-it")
	} else {
		args = append(args, "-i")
	}
	if opt.User != "" {
		args = append(args, "-u", opt.User)
	}
	if opt.Workdir != "" {
		args = append(args, "-w", opt.Workdir)
	}
	for _, env := range opt.Env {
		if env != "" {
			args = append(args, "-e", env)
		}
	}

	args = append(args, cfg.ContainerName(service))
	args = append(args, command...)

	return RunWSLCInteractive(ctx, args...)
}

func Run(ctx context.Context, cfg config.ComposeFile, service string, command []string, opts ...RunOptions) error {
	var opt RunOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	svc, ok := cfg.Services[service]
	if !ok {
		return fmt.Errorf("service %q not found in compose file", service)
	}

	if svc.Image == "" && svc.Build.IsZero() {
		return fmt.Errorf("service %q has neither image nor build specified", service)
	}

	networkNames := managedNetworkNames(cfg.Networks)
	volumeNames := managedVolumeNames(cfg.Volumes)

	if err := createManagedNetworks(ctx, cfg, networkNames, nil, nil); err != nil {
		return err
	}
	if err := createManagedVolumes(ctx, cfg, volumeNames, nil, nil); err != nil {
		return err
	}

	if !opt.NoDeps && len(svc.DependsOn) > 0 {
		deps := svc.DependsOn.SortedNames()
		if err := Up(ctx, cfg, UpOptions{
			Detach:     true,
			NoRecreate: true,
			Services:   deps,
		}); err != nil {
			return err
		}
	}

	puller := newImagePuller()
	image, err := prepareServiceImage(ctx, cfg, service, svc, UpOptions{}, "svc:"+service, nil, puller)
	if err != nil {
		return err
	}

	oneOffName := fmt.Sprintf("%s-%s-run-%s", cfg.ProjectName(), service, randomHex(4))
	overrides := containerRunOverrides{
		Detach:        opt.Detach,
		AutoRemove:    opt.Remove,
		Interactive:   !opt.Detach,
		AllocateTTY:   !opt.Detach && isStdinTTY(),
		SkipPorts:     true,
		Workdir:       opt.Workdir,
		User:          opt.User,
		Entrypoint:    opt.Entrypoint,
		HasEntrypoint: opt.HasEntrypoint,
		Env:           opt.Env,
		Command:       command,
		HasCommand:    len(command) > 0,
	}

	attachments := resolveNetworkAttachments(cfg, service, svc)
	if len(attachments) <= 1 {
		args := buildContainerRunArgs(cfg, service, oneOffName, svc, image, overrides)
		if opt.Detach {
			out, runErr := RunWSLC(ctx, args...)
			if runErr != nil {
				return fmt.Errorf("failed to run service %q: %w", service, runErr)
			}
			if out != "" {
				fmt.Fprintln(os.Stdout, out)
			} else {
				fmt.Fprintln(os.Stdout, oneOffName)
			}
			return nil
		}
		return RunWSLCInteractive(ctx, args...)
	}

	createArgs := buildContainerCreateArgs(cfg, service, oneOffName, svc, image, &attachments[0], overrides)
	res, createErr := RunWSLC(ctx, createArgs...)
	if createErr != nil {
		return fmt.Errorf("failed to create container for service %q: %w", service, createErr)
	}

	target := strings.TrimSpace(res)
	if target == "" {
		target = oneOffName
	}

	if opt.Remove {
		defer func() {
			_ = StopAndRemoveContainer(context.Background(), target)
		}()
	}

	for _, att := range attachments[1:] {
		if err := ConnectNetwork(ctx, att, target); err != nil {
			if !opt.Remove {
				_ = StopAndRemoveContainer(context.Background(), target)
			}
			return err
		}
	}

	if opt.Detach {
		if err := StartExistingContainer(ctx, target); err != nil {
			if !opt.Remove {
				_ = StopAndRemoveContainer(context.Background(), target)
			}
			return err
		}
		fmt.Fprintln(os.Stdout, target)
		return nil
	}

	return RunWSLCInteractive(ctx, "start", "-a", "-i", target)
}

func Config(cfg config.ComposeFile, opts ...ConfigOptions) error {
	var opt ConfigOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if _, err := BuildFloors(cfg.Services); err != nil {
		return err
	}
	if err := validateServices(cfg.Services); err != nil {
		return err
	}

	if opt.Quiet {
		return nil
	}

	if opt.Services {
		names := make([]string, 0, len(cfg.Services))
		for name := range cfg.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintln(os.Stdout, name)
		}
		return nil
	}

	if opt.Volumes {
		names := make([]string, 0, len(cfg.Volumes))
		for name := range cfg.Volumes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintln(os.Stdout, name)
		}
		return nil
	}

	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to render compose configuration: %w", err)
	}
	_, err = os.Stdout.Write(raw)
	return err
}

func Rm(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...RmOptions) error {
	var opt RmOptions
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

	byName := make(map[string]ContainerInfo, len(containers))
	for _, c := range containers {
		if c.Name != "" {
			byName[c.Name] = c
		}
	}

	type rmTarget struct {
		service       string
		containerName string
		running       bool
	}
	var targets []rmTarget

	for _, svcName := range names {
		cName := cfg.ContainerName(svcName)
		c, ok := byName[cName]
		if !ok {
			continue
		}
		st := parseStateFromStatusText(c.Status)
		if st.Status == "running" && !opt.Stop {
			continue
		}
		targets = append(targets, rmTarget{
			service:       svcName,
			containerName: cName,
			running:       st.Status == "running",
		})
	}

	if len(targets) == 0 {
		fmt.Fprintln(os.Stdout, "No stopped containers")
		return nil
	}

	if !opt.Force {
		containerNames := make([]string, 0, len(targets))
		for _, t := range targets {
			containerNames = append(containerNames, t.containerName)
		}
		fmt.Fprintf(os.Stdout, "Going to remove %s\nAre you sure? [yN] ", strings.Join(containerNames, ", "))
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return nil
		}
	}

	ui := newProgressUI("Removing")
	for _, t := range targets {
		ui.add("svc:"+t.service, "Container", t.containerName)
	}
	ui.start()

	var (
		mu   sync.Mutex
		errs []error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, item := range targets {
		t := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "svc:" + t.service
			if t.running && opt.Stop {
				ui.setRunning(id, "Stopping")
				if stopErr := StopContainer(ctx, t.containerName, resolveStopTimeout(-1, cfg.Services[t.service])); stopErr != nil {
					ui.setFailed(id, "Error")
					recordErr(stopErr)
					return
				}
			}
			ui.setRunning(id, "Removing")
			if rmErr := RemoveContainer(ctx, t.containerName, opt.Force || opt.Stop, opt.Volumes); rmErr != nil {
				ui.setFailed(id, "Error")
				recordErr(rmErr)
				return
			}
			ui.setDone(id, "Removed")
		}()
	}
	wg.Wait()

	ui.finish()
	return errors.Join(errs...)
}

func Images(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...ImagesOptions) error {
	var opt ImagesOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}

	localImages, _ := ListImages(ctx)

	findLocal := func(ref string) (ImageInfo, bool) {
		repo, tag := splitImageRef(ref)
		normRepo := trimRegistryPrefix(repo)
		for _, img := range localImages {
			if trimRegistryPrefix(img.Repository) == normRepo && img.Tag == tag {
				return img, true
			}
		}
		return ImageInfo{}, false
	}

	if opt.Quiet {
		seen := make(map[string]bool)
		for _, name := range names {
			svc := cfg.Services[name]
			ref := serviceImageTag(cfg, name, svc)
			id := ref
			if info, ok := findLocal(ref); ok && info.ID != "" {
				id = info.ID
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			fmt.Fprintln(os.Stdout, id)
		}
		return nil
	}

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		svc := cfg.Services[name]
		ref := serviceImageTag(cfg, name, svc)
		repo, tag := splitImageRef(ref)
		imgID := "<none>"
		size := "-"
		if info, ok := findLocal(ref); ok {
			if info.Repository != "" {
				repo = info.Repository
			}
			if info.Tag != "" {
				tag = info.Tag
			}
			if info.ID != "" {
				imgID = info.ID
			}
			if info.Size != "" {
				size = info.Size
			}
		}
		rows = append(rows, []string{cfg.ContainerName(name), repo, tag, imgID, size})
	}

	printTable(os.Stdout, []string{"CONTAINER", "REPOSITORY", "TAG", "IMAGE ID", "SIZE"}, rows)
	return nil
}

func findOrphanContainers(ctx context.Context, cfg config.ComposeFile) []ContainerInfo {
	containers, err := ListContainers(ctx)
	if err != nil {
		return nil
	}

	known := make(map[string]bool, len(cfg.Services))
	for name := range cfg.Services {
		known[cfg.ContainerName(name)] = true
	}

	proj := cfg.ProjectName()
	prefix := proj + "-"

	var orphans []ContainerInfo
	for _, c := range containers {
		if c.Name == "" || known[c.Name] {
			continue
		}
		belongs := false
		if c.Labels != nil && c.Labels[labelComposeProject] == proj {
			belongs = true
		} else if strings.HasPrefix(c.Name, prefix) && strings.HasSuffix(c.Name, "-1") {
			belongs = true
		}
		if belongs {
			orphans = append(orphans, c)
		}
	}

	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i].Name < orphans[j].Name
	})
	return orphans
}

func removeOrphanContainers(ctx context.Context, orphans []ContainerInfo, ui *progressUI, recordErr func(error)) {
	var wg sync.WaitGroup
	for _, item := range orphans {
		c := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "orphan:" + c.Name
			if ui != nil {
				ui.setRunning(id, "Removing")
			}
			if err := StopAndRemoveContainer(ctx, c.Name); err != nil {
				if ui != nil {
					ui.setFailed(id, "Error")
				}
				if recordErr != nil {
					recordErr(err)
				}
				return
			}
			if ui != nil {
				ui.setDone(id, "Removed")
			}
		}()
	}
	wg.Wait()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}
