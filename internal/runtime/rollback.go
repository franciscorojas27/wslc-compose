package runtime

import (
	"context"
	"sort"
	"sync"
	"time"
	"wslc-compose/internal/config"
)

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
