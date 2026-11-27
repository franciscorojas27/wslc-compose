package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"wslc-compose/internal/config"

	"golang.org/x/sync/errgroup"
)

type UpOptions struct {
	Detach        bool
	Build         bool
	NoBuild       bool
	ForceRecreate bool
	NoRecreate    bool
	NoDeps        bool
	RemoveOrphans bool
	Wait          bool
	Services      []string
}

type DownOptions struct {
	RemoveVolumes bool
	RemoveOrphans bool
	Timeout       int
	HasTimeout    bool
	RMI           string
	Profiles      []string
}

type PullOptions struct {
	IgnorePullFailures bool
	Quiet              bool
}

type StopOptions struct {
	Timeout int
}

func Up(ctx context.Context, cfg config.ComposeFile, opts ...UpOptions) error {
	var opt UpOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.Build && opt.NoBuild {
		return fmt.Errorf("cannot combine --build and --no-build")
	}
	if opt.ForceRecreate && opt.NoRecreate {
		return fmt.Errorf("cannot combine --force-recreate and --no-recreate")
	}

	var (
		selectedNames []string
		err           error
	)
	if opt.NoDeps && len(opt.Services) > 0 {
		selectedNames, err = selectServices(cfg.Services, opt.Services)
		if err != nil {
			return err
		}
		sort.Strings(selectedNames)
	} else {
		selectedNames, err = expandServiceDependencies(cfg.Services, opt.Services)
		if err != nil {
			return err
		}
	}

	selectedSet := make(map[string]bool, len(selectedNames))
	for _, name := range selectedNames {
		selectedSet[name] = true
	}

	selectedServices := make(map[string]config.Service, len(selectedNames))
	for _, name := range selectedNames {
		svc := cfg.Services[name]
		if opt.NoDeps && len(opt.Services) > 0 && len(svc.DependsOn) > 0 {
			filteredDeps := make(config.DependsOnConfig)
			for dep, depCfg := range svc.DependsOn {
				if selectedSet[dep] {
					filteredDeps[dep] = depCfg
				}
			}
			svc.DependsOn = filteredDeps
		}
		selectedServices[name] = svc
	}

	floors, err := BuildFloors(selectedServices)
	if err != nil {
		return err
	}

	if err := validateServices(selectedServices); err != nil {
		return err
	}
	if err := validateServicesBindMounts(cfg, selectedServices); err != nil {
		return err
	}

	effectiveCfg := withDefaultNetwork(cfg, selectedServices)
	waitConditions := computeWaitConditions(selectedServices, opt.Wait)
	networkNames := managedNetworkNames(effectiveCfg.Networks)
	volumeNames := managedVolumeNames(effectiveCfg.Volumes)

	var orphans []ContainerInfo
	if opt.RemoveOrphans {
		orphans = findOrphanContainers(ctx, effectiveCfg)
	}

	ui := newProgressUI("Running")
	for _, orphan := range orphans {
		ui.add("orphan:"+orphan.Name, "Container", orphan.Name)
	}
	for _, key := range networkNames {
		ui.add("net:"+key, "Network", effectiveCfg.NetworkName(key))
	}
	for _, key := range volumeNames {
		ui.add("vol:"+key, "Volume", effectiveCfg.VolumeName(key))
	}
	for _, floor := range floors {
		for _, svcName := range floor {
			ui.add("svc:"+svcName, "Container", effectiveCfg.ContainerName(svcName))
		}
	}
	ui.start()

	if len(orphans) > 0 {
		var (
			orphanMu   sync.Mutex
			orphanErrs []error
		)
		removeOrphanContainers(ctx, orphans, false, ui, func(err error) {
			if err == nil {
				return
			}
			orphanMu.Lock()
			orphanErrs = append(orphanErrs, err)
			orphanMu.Unlock()
		})
		if err := errors.Join(orphanErrs...); err != nil {
			ui.finish()
			return err
		}
	}

	state := newRollbackState()
	puller := newImagePuller()

	if err := createManagedNetworks(ctx, effectiveCfg, networkNames, ui, state); err != nil {
		ui.finish()
		rollback(effectiveCfg, floors, state)
		return err
	}

	if err := createManagedVolumes(ctx, effectiveCfg, volumeNames, ui, state); err != nil {
		ui.finish()
		rollback(effectiveCfg, floors, state)
		return err
	}

	for _, floor := range floors {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range floor {
			name := svcName
			svc := selectedServices[name]
			cond := waitConditions[name]
			g.Go(func() error {
				return startFloorService(gctx, effectiveCfg, name, svc, opt, cond, ui, state, puller)
			})
		}
		if err := g.Wait(); err != nil {
			ui.finish()
			rollback(effectiveCfg, floors, state)
			return err
		}
	}

	ui.finish()

	if !opt.Detach {
		return attachAndWaitForeground(ctx, effectiveCfg, selectedNames, floors)
	}

	return nil
}

func attachAndWaitForeground(ctx context.Context, cfg config.ComposeFile, services []string, floors [][]string) error {
	if len(services) == 0 {
		return nil
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var logsErr error
	out := stdout()
	if sigCtx.Err() == nil {
		fmt.Fprintf(out, "Attaching to %s\n", strings.Join(services, ", "))
		logsErr = Logs(sigCtx, cfg, services, LogsOptions{Follow: true})
	}

	interrupted := sigCtx.Err() != nil
	stopSignals()

	if !interrupted {
		return logsErr
	}

	fmt.Fprintln(out, "Gracefully stopping...")
	stopTimeout := computeShutdownTimeout(cfg, floors, nil)
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()

	stopErr := stopFloors(stopCtx, cfg, floors, -1)
	if stopErr != nil {
		return stopErr
	}
	if logsErr != nil && !errors.Is(logsErr, context.Canceled) {
		return logsErr
	}
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
		if opt.Timeout == 0 && !opt.HasTimeout {
			opt.Timeout = -1
		}
	}

	if opt.RMI != "" && opt.RMI != "all" && opt.RMI != "local" {
		return fmt.Errorf("invalid --rmi value %q: must be \"all\" or \"local\"", opt.RMI)
	}

	downCfg := cfg
	if len(opt.Profiles) > 0 && len(downCfg.ActiveProfiles) == 0 {
		downCfg.ActiveProfiles = append([]string(nil), opt.Profiles...)
	}
	if len(downCfg.ActiveProfiles) == 0 && len(downCfg.AllServices) > len(downCfg.Services) {
		downCfg.Services = downCfg.AllServices
	}

	floors, err := BuildFloors(downCfg.Services)
	if err != nil {
		return err
	}

	effectiveCfg := withDefaultNetwork(downCfg, downCfg.Services)

	var orphans []ContainerInfo
	if opt.RemoveOrphans {
		orphans = findOrphanContainers(ctx, effectiveCfg)
	}

	var volumeNames []string
	if opt.RemoveVolumes {
		volumeNames = managedVolumeNames(effectiveCfg.Volumes)
	}
	networkNames := managedNetworkNames(effectiveCfg.Networks)
	imagesToRemove := selectRMIImages(effectiveCfg, opt.RMI)

	ui := newProgressUI("Stopping")
	for i := len(floors) - 1; i >= 0; i-- {
		for _, svcName := range floors[i] {
			ui.add("svc:"+svcName, "Container", effectiveCfg.ContainerName(svcName))
		}
	}
	for _, orphan := range orphans {
		ui.add("orphan:"+orphan.Name, "Container", orphan.Name)
	}
	for _, key := range volumeNames {
		ui.add("vol:"+key, "Volume", effectiveCfg.VolumeName(key))
	}
	for _, key := range networkNames {
		ui.add("net:"+key, "Network", effectiveCfg.NetworkName(key))
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

	skipGracefulStop := opt.HasTimeout && opt.Timeout == 0
	for i := len(floors) - 1; i >= 0; i-- {
		var wg sync.WaitGroup
		for _, svcName := range floors[i] {
			name := svcName
			containerName := effectiveCfg.ContainerName(name)
			svcTimeout := resolveStopTimeout(opt.Timeout, effectiveCfg.Services[name])
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := "svc:" + name
				ui.setRunning(id, "Stopping")
				if !skipGracefulStop {
					if err := StopContainer(ctx, containerName, svcTimeout); err != nil {
						ui.setFailed(id, "Error")
						recordErr(err)
						return
					}
				}
				if err := RemoveContainer(ctx, containerName, true, opt.RemoveVolumes); err != nil {
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
		removeOrphanContainers(ctx, orphans, opt.RemoveVolumes, ui, recordErr)
	}

	if len(volumeNames) > 0 {
		var volWG sync.WaitGroup
		for _, key := range volumeNames {
			volKey := key
			vol := effectiveCfg.Volumes[volKey]
			realName := effectiveCfg.VolumeName(volKey)
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
		net := effectiveCfg.Networks[netKey]
		realName := effectiveCfg.NetworkName(netKey)
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

func Start(ctx context.Context, cfg config.ComposeFile, targetServices []string) error {
	floors, err := BuildFloors(cfg.Services)
	if err != nil {
		return err
	}

	filtered, err := filterFloors(floors, cfg.Services, targetServices)
	if err != nil {
		return err
	}

	selectedServices := make(map[string]config.Service)
	for _, floor := range filtered {
		for _, name := range floor {
			selectedServices[name] = cfg.Services[name]
		}
	}
	waitConditions := computeWaitConditions(selectedServices, false)

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
			svc := cfg.Services[name]
			cond := waitConditions[name]
			g.Go(func() error {
				id := "svc:" + name
				ui.setRunning(id, "Starting")
				if err := StartExistingContainer(gctx, containerName); err != nil {
					ui.setFailed(id, "Error")
					return err
				}
				if cond != "" && cond != "service_started" {
					ui.setRunning(id, "Waiting")
					if err := WaitForCondition(gctx, containerName, svc, cond); err != nil {
						ui.setFailed(id, "Unhealthy")
						return err
					}
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

	var existingFloors [][]string
	selectedServices := make(map[string]config.Service)
	for _, floor := range filtered {
		var level []string
		for _, name := range floor {
			st, stErr := InspectContainerState(ctx, cfg.ContainerName(name))
			if stErr == nil && !st.Exists {
				continue
			}
			level = append(level, name)
			selectedServices[name] = cfg.Services[name]
		}
		if len(level) > 0 {
			existingFloors = append(existingFloors, level)
		}
	}

	if len(existingFloors) == 0 {
		return nil
	}

	waitConditions := computeWaitConditions(selectedServices, false)

	ui := newProgressUI("Restarting")
	for _, floor := range existingFloors {
		for _, name := range floor {
			ui.add("svc:"+name, "Container", cfg.ContainerName(name))
		}
	}
	ui.start()

	for i := len(existingFloors) - 1; i >= 0; i-- {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range existingFloors[i] {
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

	for _, floor := range existingFloors {
		g, gctx := errgroup.WithContext(ctx)
		for _, svcName := range floor {
			name := svcName
			containerName := cfg.ContainerName(name)
			svc := cfg.Services[name]
			cond := waitConditions[name]
			g.Go(func() error {
				id := "svc:" + name
				ui.setRunning(id, "Restarting")
				if err := StartExistingContainer(gctx, containerName); err != nil {
					if errors.Is(err, ErrNotFound) {
						ui.setDone(id, "Skipped")
						return nil
					}
					ui.setFailed(id, "Error")
					return err
				}
				if cond != "" && cond != "service_started" {
					ui.setRunning(id, "Waiting")
					if err := WaitForCondition(gctx, containerName, svc, cond); err != nil {
						ui.setFailed(id, "Unhealthy")
						return err
					}
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
