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
	NoDeps        bool
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
	if len(services) == 0 {
		return nil
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	out := stdout()
	if sigCtx.Err() == nil {
		fmt.Fprintf(out, "Attaching to %s\n", strings.Join(services, ", "))
		_ = Logs(sigCtx, cfg, services, LogsOptions{Follow: true})
	}

	interrupted := sigCtx.Err() != nil
	stopSignals()

	if !interrupted {
		return nil
	}

	fmt.Fprintln(out, "Gracefully stopping...")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return stopFloors(stopCtx, cfg, floors, -1)
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
