package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"wslc-compose/internal/config"

	"golang.org/x/sync/errgroup"
)

type UpOptions struct {
	Detach        bool
	Build         bool
	NoBuild       bool
	ForceRecreate bool
	NoRecreate    bool
	RemoveOrphans bool
	Wait          bool
	Services      []string
}

type DownOptions struct {
	RemoveVolumes bool
	RemoveOrphans bool
	Timeout       int
	RMI           string
}

type PullOptions struct {
	IgnorePullFailures bool
	Quiet              bool
}

type StopOptions struct {
	Timeout int
}

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

type rollbackState struct {
	mu         sync.Mutex
	networks   []string
	volumes    []string
	containers map[string]bool
}

func newRollbackState() *rollbackState {
	return &rollbackState{
		containers: make(map[string]bool),
	}
}

func (s *rollbackState) addNetwork(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networks = append(s.networks, name)
}

func (s *rollbackState) addVolume(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes = append(s.volumes, name)
}

func (s *rollbackState) addContainer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers[name] = true
}

func (s *rollbackState) isEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.networks) == 0 && len(s.volumes) == 0 && len(s.containers) == 0
}

func Up(ctx context.Context, cfg config.ComposeFile, opts ...UpOptions) error {
	opt := UpOptions{Detach: true}
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.Build && opt.NoBuild {
		return fmt.Errorf("cannot combine --build and --no-build")
	}
	if opt.ForceRecreate && opt.NoRecreate {
		return fmt.Errorf("cannot combine --force-recreate and --no-recreate")
	}

	selectedNames, err := expandServiceDependencies(cfg.Services, opt.Services)
	if err != nil {
		return err
	}

	selectedServices := make(map[string]config.Service, len(selectedNames))
	for _, name := range selectedNames {
		selectedServices[name] = cfg.Services[name]
	}

	floors, err := BuildFloors(selectedServices)
	if err != nil {
		return err
	}

	if err := validateServices(selectedServices); err != nil {
		return err
	}

	waitConditions := computeWaitConditions(selectedServices, opt.Wait)
	networkNames := managedNetworkNames(cfg.Networks)
	volumeNames := managedVolumeNames(cfg.Volumes)

	var orphans []ContainerInfo
	if opt.RemoveOrphans {
		orphans = findOrphanContainers(ctx, cfg)
	}

	ui := newProgressUI("Running")
	for _, orphan := range orphans {
		ui.add("orphan:"+orphan.Name, "Container", orphan.Name)
	}
	for _, key := range networkNames {
		ui.add("net:"+key, "Network", cfg.NetworkName(key))
	}
	for _, key := range volumeNames {
		ui.add("vol:"+key, "Volume", cfg.VolumeName(key))
	}
	for _, floor := range floors {
		for _, svcName := range floor {
			ui.add("svc:"+svcName, "Container", cfg.ContainerName(svcName))
		}
	}
	ui.start()

	if len(orphans) > 0 {
		removeOrphanContainers(ctx, orphans, ui, nil)
	}

	state := newRollbackState()
	puller := newImagePuller()

	if err := createManagedNetworks(ctx, cfg, networkNames, ui, state); err != nil {
		ui.finish()
		rollback(cfg, floors, state)
		return err
	}

	if err := createManagedVolumes(ctx, cfg, volumeNames, ui, state); err != nil {
		ui.finish()
		rollback(cfg, floors, state)
		return err
	}

	for _, floor := range floors {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range floor {
			name := svcName
			svc := selectedServices[name]
			cond := waitConditions[name]
			g.Go(func() error {
				return startFloorService(gctx, cfg, name, svc, opt, cond, ui, state, puller)
			})
		}
		if err := g.Wait(); err != nil {
			ui.finish()
			rollback(cfg, floors, state)
			return err
		}
	}

	ui.finish()

	if !opt.Detach {
		return attachAndWaitForeground(ctx, cfg, selectedNames, floors)
	}

	return nil
}

func attachAndWaitForeground(ctx context.Context, cfg config.ComposeFile, services []string, floors [][]string) error {
	fmt.Fprintf(os.Stdout, "Attaching to %s\n", strings.Join(services, ", "))
	_ = Logs(ctx, cfg, services, LogsOptions{Follow: true})

	fmt.Fprintln(os.Stdout, "Gracefully stopping...")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stopFloors(stopCtx, cfg, floors, -1)
	return nil
}

func stopFloors(ctx context.Context, cfg config.ComposeFile, floors [][]string, timeout int) error {
	ui := newProgressUI("Stopping")
	for i := len(floors) - 1; i >= 0; i-- {
		for _, svcName := range floors[i] {
			ui.add("svc:"+svcName, "Container", cfg.ContainerName(svcName))
		}
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

	for i := len(floors) - 1; i >= 0; i-- {
		var wg sync.WaitGroup
		for _, svcName := range floors[i] {
			name := svcName
			containerName := cfg.ContainerName(name)
			svcTimeout := resolveStopTimeout(timeout, cfg.Services[name])
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := "svc:" + name
				ui.setRunning(id, "Stopping")
				if err := StopContainer(ctx, containerName, svcTimeout); err != nil {
					ui.setFailed(id, "Error")
					recordErr(err)
					return
				}
				ui.setDone(id, "Stopped")
			}()
		}
		wg.Wait()
	}

	ui.finish()
	return errors.Join(errs...)
}

func Down(ctx context.Context, cfg config.ComposeFile, opts ...DownOptions) error {
	opt := DownOptions{Timeout: -1}
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.RMI != "" && opt.RMI != "all" && opt.RMI != "local" {
		return fmt.Errorf("invalid --rmi value %q: must be \"all\" or \"local\"", opt.RMI)
	}

	floors, err := BuildFloors(cfg.Services)
	if err != nil {
		return err
	}

	var orphans []ContainerInfo
	if opt.RemoveOrphans {
		orphans = findOrphanContainers(ctx, cfg)
	}

	var volumeNames []string
	if opt.RemoveVolumes {
		volumeNames = managedVolumeNames(cfg.Volumes)
	}
	networkNames := managedNetworkNames(cfg.Networks)
	imagesToRemove := selectRMIImages(cfg, opt.RMI)

	ui := newProgressUI("Stopping")
	for i := len(floors) - 1; i >= 0; i-- {
		for _, svcName := range floors[i] {
			ui.add("svc:"+svcName, "Container", cfg.ContainerName(svcName))
		}
	}
	for _, orphan := range orphans {
		ui.add("orphan:"+orphan.Name, "Container", orphan.Name)
	}
	for _, key := range volumeNames {
		ui.add("vol:"+key, "Volume", cfg.VolumeName(key))
	}
	for _, key := range networkNames {
		ui.add("net:"+key, "Network", cfg.NetworkName(key))
	}
	for _, img := range imagesToRemove {
		ui.add("img:"+img, "Image", img)
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

	for i := len(floors) - 1; i >= 0; i-- {
		var wg sync.WaitGroup
		for _, svcName := range floors[i] {
			name := svcName
			containerName := cfg.ContainerName(name)
			svcTimeout := resolveStopTimeout(opt.Timeout, cfg.Services[name])
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := "svc:" + name
				ui.setRunning(id, "Stopping")
				if svcTimeout >= 0 {
					if err := StopContainer(ctx, containerName, svcTimeout); err != nil {
						ui.setFailed(id, "Error")
						recordErr(err)
						return
					}
				}
				if err := StopAndRemoveContainer(ctx, containerName); err != nil {
					ui.setFailed(id, "Error")
					recordErr(err)
					return
				}
				ui.setDone(id, "Removed")
			}()
		}
		wg.Wait()
	}

	if len(orphans) > 0 {
		removeOrphanContainers(ctx, orphans, ui, recordErr)
	}

	if len(volumeNames) > 0 {
		var volWG sync.WaitGroup
		for _, key := range volumeNames {
			volKey := key
			vol := cfg.Volumes[volKey]
			realName := cfg.VolumeName(volKey)
			volWG.Add(1)
			go func() {
				defer volWG.Done()
				id := "vol:" + volKey
				ui.setRunning(id, "Removing")
				if _, err := RemoveVolume(ctx, realName, vol); err != nil {
					ui.setFailed(id, "Error")
					recordErr(err)
					return
				}
				ui.setDone(id, "Removed")
			}()
		}
		volWG.Wait()
	}

	var netWG sync.WaitGroup
	for _, key := range networkNames {
		netKey := key
		net := cfg.Networks[netKey]
		realName := cfg.NetworkName(netKey)
		netWG.Add(1)
		go func() {
			defer netWG.Done()
			id := "net:" + netKey
			ui.setRunning(id, "Removing")
			if _, err := RemoveNetwork(ctx, realName, net); err != nil {
				ui.setFailed(id, "Error")
				recordErr(err)
				return
			}
			ui.setDone(id, "Removed")
		}()
	}
	netWG.Wait()

	if len(imagesToRemove) > 0 {
		var imgWG sync.WaitGroup
		for _, image := range imagesToRemove {
			img := image
			imgWG.Add(1)
			go func() {
				defer imgWG.Done()
				id := "img:" + img
				ui.setRunning(id, "Removing")
				if err := RemoveImage(ctx, img); err != nil {
					ui.setFailed(id, "Error")
					recordErr(err)
					return
				}
				ui.setDone(id, "Removed")
			}()
		}
		imgWG.Wait()
	}

	ui.finish()
	return errors.Join(errs...)
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
		opt.ProjectDir = cfg.ProjectDir
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

func Start(ctx context.Context, cfg config.ComposeFile, targetServices []string) error {
	floors, err := BuildFloors(cfg.Services)
	if err != nil {
		return err
	}

	filtered, err := filterFloors(floors, cfg.Services, targetServices)
	if err != nil {
		return err
	}

	ui := newProgressUI("Starting")
	for _, floor := range filtered {
		for _, name := range floor {
			ui.add("svc:"+name, "Container", cfg.ContainerName(name))
		}
	}
	ui.start()

	for _, floor := range filtered {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range floor {
			name := svcName
			containerName := cfg.ContainerName(name)
			g.Go(func() error {
				id := "svc:" + name
				ui.setRunning(id, "Starting")
				if err := StartExistingContainer(gctx, containerName); err != nil {
					ui.setFailed(id, "Error")
					return err
				}
				ui.setDone(id, "Started")
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			ui.finish()
			return err
		}
	}

	ui.finish()
	return nil
}

func Stop(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...StopOptions) error {
	timeout := -1
	if len(opts) > 0 {
		timeout = opts[0].Timeout
	}

	floors, err := BuildFloors(cfg.Services)
	if err != nil {
		return err
	}

	filtered, err := filterFloors(floors, cfg.Services, targetServices)
	if err != nil {
		return err
	}

	return stopFloors(ctx, cfg, filtered, timeout)
}

func Restart(ctx context.Context, cfg config.ComposeFile, targetServices []string, opts ...StopOptions) error {
	timeout := -1
	if len(opts) > 0 {
		timeout = opts[0].Timeout
	}

	floors, err := BuildFloors(cfg.Services)
	if err != nil {
		return err
	}

	filtered, err := filterFloors(floors, cfg.Services, targetServices)
	if err != nil {
		return err
	}

	ui := newProgressUI("Restarting")
	for _, floor := range filtered {
		for _, name := range floor {
			ui.add("svc:"+name, "Container", cfg.ContainerName(name))
		}
	}
	ui.start()

	for i := len(filtered) - 1; i >= 0; i-- {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range filtered[i] {
			name := svcName
			containerName := cfg.ContainerName(name)
			svcTimeout := resolveStopTimeout(timeout, cfg.Services[name])
			g.Go(func() error {
				id := "svc:" + name
				ui.setRunning(id, "Restarting")
				if err := StopContainer(gctx, containerName, svcTimeout); err != nil {
					ui.setFailed(id, "Error")
					return err
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			ui.finish()
			return err
		}
	}

	for _, floor := range filtered {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range floor {
			name := svcName
			containerName := cfg.ContainerName(name)
			g.Go(func() error {
				id := "svc:" + name
				ui.setRunning(id, "Restarting")
				if err := StartExistingContainer(gctx, containerName); err != nil {
					ui.setFailed(id, "Error")
					return err
				}
				ui.setDone(id, "Restarted")
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			ui.finish()
			return err
		}
	}

	ui.finish()
	return nil
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

func startFloorService(ctx context.Context, cfg config.ComposeFile, name string, svc config.Service, opt UpOptions, waitCond string, ui *progressUI, state *rollbackState, puller *imagePuller) error {
	id := "svc:" + name
	containerName := cfg.ContainerName(name)

	if opt.NoRecreate {
		st, err := InspectContainerState(ctx, containerName)
		if err == nil && st.Exists {
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
			if err := BuildImage(ctx, image, svc.Build, BuildOptions{ProjectDir: cfg.ProjectDir}); err != nil {
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

func rollback(cfg config.ComposeFile, floors [][]string, state *rollbackState) {
	if state.isEmpty() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	state.mu.Lock()
	containers := make(map[string]bool, len(state.containers))
	for name := range state.containers {
		containers[name] = true
	}
	volumes := append([]string(nil), state.volumes...)
	networks := append([]string(nil), state.networks...)
	state.mu.Unlock()

	sort.Strings(volumes)
	sort.Strings(networks)

	ui := newProgressUI("Rolling back")
	for i := len(floors) - 1; i >= 0; i-- {
		for _, svcName := range floors[i] {
			if containers[svcName] {
				ui.add("svc:"+svcName, "Container", cfg.ContainerName(svcName))
			}
		}
	}
	for _, key := range volumes {
		ui.add("vol:"+key, "Volume", cfg.VolumeName(key))
	}
	for _, key := range networks {
		ui.add("net:"+key, "Network", cfg.NetworkName(key))
	}
	ui.start()

	for i := len(floors) - 1; i >= 0; i-- {
		var wg sync.WaitGroup
		for _, svcName := range floors[i] {
			if !containers[svcName] {
				continue
			}
			name := svcName
			containerName := cfg.ContainerName(name)
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := "svc:" + name
				ui.setRunning(id, "Removing")
				if err := StopAndRemoveContainer(ctx, containerName); err != nil {
					ui.setFailed(id, "Error")
					return
				}
				ui.setDone(id, "Removed")
			}()
		}
		wg.Wait()
	}

	var volWG sync.WaitGroup
	for _, key := range volumes {
		volKey := key
		vol := cfg.Volumes[volKey]
		realName := cfg.VolumeName(volKey)
		volWG.Add(1)
		go func() {
			defer volWG.Done()
			id := "vol:" + volKey
			ui.setRunning(id, "Removing")
			if _, err := RemoveVolume(ctx, realName, vol); err != nil {
				ui.setFailed(id, "Error")
				return
			}
			ui.setDone(id, "Removed")
		}()
	}
	volWG.Wait()

	var netWG sync.WaitGroup
	for _, key := range networks {
		netKey := key
		net := cfg.Networks[netKey]
		realName := cfg.NetworkName(netKey)
		netWG.Add(1)
		go func() {
			defer netWG.Done()
			id := "net:" + netKey
			ui.setRunning(id, "Removing")
			if _, err := RemoveNetwork(ctx, realName, net); err != nil {
				ui.setFailed(id, "Error")
				return
			}
			ui.setDone(id, "Removed")
		}()
	}
	netWG.Wait()

	ui.finish()
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
			if svc.HealthCheck != nil && !svc.HealthCheck.Disable && len(svc.HealthCheck.Test) > 0 {
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

	seen := make(map[string]bool)
	var images []string

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := cfg.Services[name]
		if mode == "local" && svc.Image != "" && svc.Build.IsZero() {
			continue
		}
		img := serviceImageTag(cfg, name, svc)
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
	return -1
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
