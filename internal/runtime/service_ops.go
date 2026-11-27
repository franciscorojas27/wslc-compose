package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"wslc-compose/internal/config"

	"golang.org/x/sync/errgroup"
)

type pullFlight struct {
	done     chan struct{}
	err      error
	mu       sync.Mutex
	pulling  bool
	waiters  []string
	lastProg PullProgress
	hasProg  bool
}

type imagePuller struct {
	mu      sync.Mutex
	flights map[string]*pullFlight
}

func newImagePuller() *imagePuller {
	return &imagePuller{
		flights: make(map[string]*pullFlight),
	}
}

func (p *imagePuller) ensure(ctx context.Context, image string, itemID string, ui *progressUI) error {
	p.mu.Lock()
	if flight, active := p.flights[image]; active {
		p.mu.Unlock()

		flight.mu.Lock()
		flight.waiters = append(flight.waiters, itemID)
		pulling := flight.pulling
		hasProg := flight.hasProg
		lastProg := flight.lastProg
		flight.mu.Unlock()

		if pulling && ui != nil {
			ui.setRunning(itemID, "Pulling")
			if hasProg {
				ui.setProgress(itemID, lastProg)
			}
		}

		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	flight := &pullFlight{
		done:    make(chan struct{}),
		waiters: []string{itemID},
	}
	p.flights[image] = flight
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.flights, image)
		p.mu.Unlock()
		close(flight.done)
	}()

	if ImageExists(ctx, image) {
		return nil
	}

	flight.mu.Lock()
	flight.pulling = true
	initialWaiters := append([]string(nil), flight.waiters...)
	flight.mu.Unlock()

	if ui != nil {
		for _, id := range initialWaiters {
			ui.setRunning(id, "Pulling")
		}
	}

	err := PullImage(ctx, image, func(prog PullProgress) {
		flight.mu.Lock()
		flight.lastProg = prog
		flight.hasProg = true
		waiters := append([]string(nil), flight.waiters...)
		flight.mu.Unlock()

		if ui != nil {
			for _, id := range waiters {
				ui.setProgress(id, prog)
			}
		}
	})

	flight.err = err
	return err
}

func Pull(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...PullOptions) error {
	var opt PullOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}

	imageServices := make(map[string][]string)
	var imageOrder []string
	var pullableServices []string

	for _, name := range names {
		image := cfg.Services[name].Image
		if image == "" {
			continue
		}
		pullableServices = append(pullableServices, name)
		if _, exists := imageServices[image]; !exists {
			imageOrder = append(imageOrder, image)
		}
		imageServices[image] = append(imageServices[image], name)
	}

	if len(pullableServices) == 0 {
		return nil
	}

	var ui *progressUI
	if !opt.Quiet {
		ui = newProgressUI("Pulling")
		for _, name := range pullableServices {
			ui.add("svc:"+name, "Service", name)
		}
		ui.start()
	}

	if opt.IgnorePullFailures {
		var wg sync.WaitGroup
		for _, image := range imageOrder {
			img := image
			svcs := imageServices[img]
			wg.Add(1)
			go func() {
				defer wg.Done()
				pullOneImage(ctx, img, svcs, ui)
			}()
		}
		wg.Wait()
		if ui != nil {
			ui.finish()
		}
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, image := range imageOrder {
		img := image
		svcs := imageServices[img]
		g.Go(func() error {
			return pullOneImage(gctx, img, svcs, ui)
		})
	}

	err = g.Wait()
	if ui != nil {
		ui.finish()
	}
	return err
}

func pullOneImage(ctx context.Context, img string, svcs []string, ui *progressUI) error {
	if ui != nil {
		for _, name := range svcs {
			ui.setRunning("svc:"+name, "Pulling")
		}
	}
	err := PullImage(ctx, img, func(prog PullProgress) {
		if ui != nil {
			for _, name := range svcs {
				ui.setProgress("svc:"+name, prog)
			}
		}
	})
	if err != nil {
		if ui != nil {
			status := "Error"
			if errors.Is(err, context.Canceled) {
				status = "Canceled"
			}
			for _, name := range svcs {
				ui.setFailed("svc:"+name, status)
			}
		}
		return err
	}
	if ui != nil {
		for _, name := range svcs {
			ui.setDone("svc:"+name, "Pulled")
		}
	}
	return nil
}

func Build(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...BuildOptions) error {
	var opt BuildOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.ProjectDir == "" {
		opt.ProjectDir = cfg.ComposeBaseDir()
	}

	names, err := selectServices(cfg.Services, targetServices)
	if err != nil {
		return err
	}

	var buildable []string
	for _, name := range names {
		if !cfg.Services[name].Build.IsZero() {
			buildable = append(buildable, name)
		}
	}

	if len(buildable) == 0 {
		return nil
	}

	ui := newProgressUI("Building")
	for _, name := range buildable {
		ui.add("svc:"+name, "Service", name)
	}
	ui.start()

	g, gctx := errgroup.WithContext(ctx)
	for _, svcName := range buildable {
		name := svcName
		svc := cfg.Services[name]
		g.Go(func() error {
			id := "svc:" + name
			tag := serviceImageTag(cfg, name, svc)
			ui.setRunning(id, "Building")
			if err := BuildImage(gctx, tag, svc.Build, opt); err != nil {
				ui.setFailed(id, "Error")
				return err
			}
			ui.setDone(id, "Built")
			return nil
		})
	}

	err = g.Wait()
	ui.finish()
	return err
}

func createManagedNetworks(ctx context.Context, cfg config.ComposeFile, keys []string, ui *progressUI, state *rollbackState) error {
	if len(keys) == 0 {
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, key := range keys {
		netKey := key
		net := cfg.Networks[netKey]
		realName := cfg.NetworkName(netKey)
		g.Go(func() error {
			id := "net:" + netKey
			if ui != nil {
				ui.setRunning(id, "Creating")
			}
			_, created, err := createNetwork(gctx, realName, net)
			if err != nil {
				if ui != nil {
					ui.setFailed(id, "Error")
				}
				return err
			}
			if created {
				if state != nil {
					state.addNetwork(netKey)
				}
				if ui != nil {
					ui.setDone(id, "Created")
				}
				return nil
			}
			if ui != nil {
				ui.setDone(id, "Exists")
			}
			return nil
		})
	}

	return g.Wait()
}

func createManagedVolumes(ctx context.Context, cfg config.ComposeFile, keys []string, ui *progressUI, state *rollbackState) error {
	if len(keys) == 0 {
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, key := range keys {
		volKey := key
		vol := cfg.Volumes[volKey]
		realName := cfg.VolumeName(volKey)
		g.Go(func() error {
			id := "vol:" + volKey
			if ui != nil {
				ui.setRunning(id, "Creating")
			}
			_, created, err := createVolume(gctx, realName, vol)
			if err != nil {
				if ui != nil {
					ui.setFailed(id, "Error")
				}
				return err
			}
			if created {
				if state != nil {
					state.addVolume(volKey)
				}
				if ui != nil {
					ui.setDone(id, "Created")
				}
				return nil
			}
			if ui != nil {
				ui.setDone(id, "Exists")
			}
			return nil
		})
	}

	return g.Wait()
}

func containerIsUpToDate(st ContainerState, expectedImage string, expectedHash string, currentImageID ...string) bool {
	if st.ConfigHash != "" && expectedHash != "" && st.ConfigHash != expectedHash {
		return false
	}
	if st.Image != "" && expectedImage != "" {
		stRepo, stTag := splitImageRef(st.Image)
		expRepo, expTag := splitImageRef(expectedImage)
		if trimRegistryPrefix(stRepo) != trimRegistryPrefix(expRepo) || stTag != expTag {
			return false
		}
	}
	if len(currentImageID) > 0 && st.ImageID != "" && currentImageID[0] != "" && !sameImageID(st.ImageID, currentImageID[0]) {
		return false
	}
	return true
}

func startFloorService(ctx context.Context, cfg config.ComposeFile, name string, svc config.Service, opt UpOptions, waitCond string, ui *progressUI, state *rollbackState, puller *imagePuller) error {
	id := "svc:" + name
	containerName := cfg.ContainerName(name)
	expectedImage := serviceImageTag(cfg, name, svc)
	expectedHash := computeServiceConfigHash(cfg, name, svc, expectedImage)

	if !opt.ForceRecreate {
		st, err := InspectContainerState(ctx, containerName)
		if err == nil && st.Exists {
			var currentImageID string
			if !opt.NoRecreate && !opt.Build && st.ImageID != "" {
				currentImageID = InspectImageID(ctx, expectedImage)
			}

			if waitCond == "service_completed_successfully" && st.Status == "exited" && st.ExitCode == 0 {
				if opt.NoRecreate || (!opt.Build && containerIsUpToDate(st, expectedImage, expectedHash, currentImageID)) {
					ui.setDone(id, "Completed")
					return nil
				}
			}

			if opt.NoRecreate {
				if st.Status != "running" {
					ui.setRunning(id, "Starting")
					if startErr := StartExistingContainer(ctx, containerName); startErr != nil {
						ui.setFailed(id, "Error")
						return startErr
					}
				}
				if waitCond != "" && waitCond != "service_started" {
					ui.setRunning(id, "Waiting")
					if waitErr := WaitForCondition(ctx, containerName, svc, waitCond); waitErr != nil {
						ui.setFailed(id, "Unhealthy")
						return waitErr
					}
					ui.setDone(id, "Healthy")
					return nil
				}
				if st.Status == "running" {
					ui.setDone(id, "Running")
				} else {
					ui.setDone(id, "Started")
				}
				return nil
			}

			if !opt.Build && st.Status == "running" && containerIsUpToDate(st, expectedImage, expectedHash, currentImageID) {
				if waitCond != "" && waitCond != "service_started" {
					ui.setRunning(id, "Waiting")
					if waitErr := WaitForCondition(ctx, containerName, svc, waitCond); waitErr != nil {
						ui.setFailed(id, "Unhealthy")
						return waitErr
					}
					ui.setDone(id, "Healthy")
					return nil
				}
				ui.setDone(id, "Running")
				return nil
			}
		}
	}

	image, err := prepareServiceImage(ctx, cfg, name, svc, opt, id, ui, puller)
	if err != nil {
		return err
	}

	if ctx.Err() != nil {
		ui.setFailed(id, "Canceled")
		return ctx.Err()
	}

	ui.setRunning(id, "Starting")
	state.addContainer(name)
	if _, err := runContainer(ctx, cfg, name, containerName, svc, image); err != nil {
		ui.setFailed(id, "Error")
		return err
	}

	if waitCond != "" && waitCond != "service_started" {
		ui.setRunning(id, "Waiting")
		if err := WaitForCondition(ctx, containerName, svc, waitCond); err != nil {
			ui.setFailed(id, "Unhealthy")
			return err
		}
		if waitCond == "service_healthy" {
			ui.setDone(id, "Healthy")
			return nil
		}
	}

	ui.setDone(id, "Started")
	return nil
}

func prepareServiceImage(ctx context.Context, cfg config.ComposeFile, name string, svc config.Service, opt UpOptions, itemID string, ui *progressUI, puller *imagePuller) (string, error) {
	image := serviceImageTag(cfg, name, svc)

	if !svc.Build.IsZero() {
		shouldBuild := opt.Build
		if !shouldBuild && !opt.NoBuild {
			shouldBuild = !ImageExists(ctx, image)
		}
		if shouldBuild {
			if ui != nil {
				ui.setRunning(itemID, "Building")
			}
			if err := BuildImage(ctx, image, svc.Build, BuildOptions{ProjectDir: cfg.ComposeBaseDir()}); err != nil {
				if ui != nil {
					ui.setFailed(itemID, "Error")
				}
				return "", err
			}
			return image, nil
		}

		if opt.NoBuild && !ImageExists(ctx, image) {
			if svc.Image != "" {
				if err := puller.ensure(ctx, image, itemID, ui); err != nil {
					if ui != nil {
						ui.setFailed(itemID, "Error")
					}
					return "", err
				}
				return image, nil
			}
			err := fmt.Errorf("image %q not found locally and --no-build was set", image)
			if ui != nil {
				ui.setFailed(itemID, "Error")
			}
			return "", err
		}

		return image, nil
	}

	if err := puller.ensure(ctx, image, itemID, ui); err != nil {
		if ui != nil {
			status := "Error"
			if errors.Is(err, context.Canceled) {
				status = "Canceled"
			}
			ui.setFailed(itemID, status)
		}
		return "", err
	}
	return image, nil
}

func computeWaitConditions(services map[string]config.Service, waitAll bool) map[string]string {
	conds := make(map[string]string)

	for _, svc := range services {
		for dep, depCfg := range svc.DependsOn {
			if depCfg.Condition == "service_completed_successfully" {
				conds[dep] = "service_completed_successfully"
			} else if depCfg.Condition == "service_healthy" && conds[dep] != "service_completed_successfully" {
				conds[dep] = "service_healthy"
			}
		}
	}

	if waitAll {
		for name, svc := range services {
			if !isHealthCheckDisabled(svc.HealthCheck) {
				if conds[name] == "" {
					conds[name] = "service_healthy"
				}
			}
		}
	}

	return conds
}

func serviceImageTag(cfg config.ComposeFile, name string, svc config.Service) string {
	if svc.Image != "" {
		return svc.Image
	}
	if proj := cfg.ProjectName(); proj != "" && cfg.Name != "" {
		return fmt.Sprintf("%s-%s:latest", proj, name)
	}
	return name + ":latest"
}

func selectRMIImages(cfg config.ComposeFile, mode string) []string {
	if mode == "" {
		return nil
	}

	activeCfg := cfg.FilterByProfiles(cfg.ActiveProfiles, nil)

	seen := make(map[string]bool)
	var images []string

	names := make([]string, 0, len(activeCfg.Services))
	for name := range activeCfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := activeCfg.Services[name]
		if mode == "local" && (svc.Image != "" || svc.Build.IsZero()) {
			continue
		}
		img := serviceImageTag(activeCfg, name, svc)
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		images = append(images, img)
	}

	return images
}

func resolveStopTimeout(explicit int, svc config.Service) int {
	if explicit >= 0 {
		return explicit
	}
	if secs, ok := config.ParseDurationSeconds(svc.StopGracePeriod); ok {
		return secs
	}
	return 10
}

func validateServices(services map[string]config.Service) error {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := services[name]
		if svc.Image == "" && svc.Build.IsZero() {
			return fmt.Errorf("service %q has neither image nor build specified", name)
		}
	}

	return nil
}

func validateServicesBindMounts(cfg config.ComposeFile, services map[string]config.Service) error {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := validateServiceBindMounts(cfg, name, services[name]); err != nil {
			return err
		}
	}
	return nil
}

func managedNetworkNames(networks map[string]config.Network) []string {
	var names []string
	for name, net := range networks {
		if !net.External {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func managedVolumeNames(volumes map[string]config.Volume) []string {
	var names []string
	for name, vol := range volumes {
		if !vol.External {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func selectServices(services map[string]config.Service, targets []string) ([]string, error) {
	if len(targets) == 0 {
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names)
		return names, nil
	}

	seen := make(map[string]bool, len(targets))
	names := make([]string, 0, len(targets))
	for _, name := range targets {
		if _, ok := services[name]; !ok {
			return nil, fmt.Errorf("service %q not found in compose file", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}

	return names, nil
}

func filterFloors(floors [][]string, services map[string]config.Service, targets []string) ([][]string, error) {
	if len(targets) == 0 {
		return floors, nil
	}

	selected, err := selectServices(services, targets)
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(selected))
	for _, name := range selected {
		wanted[name] = true
	}

	var filtered [][]string
	for _, floor := range floors {
		var level []string
		for _, name := range floor {
			if wanted[name] {
				level = append(level, name)
			}
		}
		if len(level) > 0 {
			filtered = append(filtered, level)
		}
	}

	return filtered, nil
}
