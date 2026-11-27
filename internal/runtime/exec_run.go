package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"wslc-compose/internal/config"
)

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

	if err := validateServiceBindMounts(cfg, service, svc); err != nil {
		return err
	}

	effectiveCfg := withDefaultNetwork(cfg, map[string]config.Service{service: svc})
	networkNames := managedNetworkNames(effectiveCfg.Networks)
	volumeNames := managedVolumeNames(effectiveCfg.Volumes)

	if err := createManagedNetworks(ctx, effectiveCfg, networkNames, nil, nil); err != nil {
		return err
	}
	if err := createManagedVolumes(ctx, effectiveCfg, volumeNames, nil, nil); err != nil {
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
		for _, dep := range deps {
			depCond := svc.DependsOn[dep].Condition
			if depCond != "" && depCond != "service_started" {
				if depSvc, exists := cfg.Services[dep]; exists {
					if err := WaitForCondition(ctx, cfg.ContainerName(dep), depSvc, depCond); err != nil {
						return err
					}
				}
			}
		}
	}

	puller := newImagePuller()
	image, err := prepareServiceImage(ctx, effectiveCfg, service, svc, UpOptions{}, "svc:"+service, nil, puller)
	if err != nil {
		return err
	}

	oneOffName := fmt.Sprintf("%s-%s-run-%s", effectiveCfg.ProjectName(), service, randomHex(4))
	overrides := containerRunOverrides{
		Detach:        opt.Detach,
		AutoRemove:    opt.Remove,
		SkipRestart:   true,
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

	attachments := resolveNetworkAttachments(effectiveCfg, service, svc)
	if len(attachments) <= 1 {
		args := buildContainerRunArgs(effectiveCfg, service, oneOffName, svc, image, overrides)
		if opt.Detach {
			out, runErr := RunWSLC(ctx, args...)
			if runErr != nil {
				return fmt.Errorf("failed to run service %q: %w", service, runErr)
			}
			if out != "" {
				fmt.Fprintln(stdout(), out)
			} else {
				fmt.Fprintln(stdout(), oneOffName)
			}
			return nil
		}
		return RunWSLCInteractive(ctx, args...)
	}

	createArgs := buildContainerCreateArgs(effectiveCfg, service, oneOffName, svc, image, &attachments[0], overrides)
	res, createErr := RunWSLC(ctx, createArgs...)
	if createErr != nil {
		return fmt.Errorf("failed to create container for service %q: %w", service, createErr)
	}

	target := strings.TrimSpace(res)
	if target == "" {
		target = oneOffName
	}

	for _, att := range attachments[1:] {
		if err := ConnectNetwork(ctx, att, target); err != nil {
			_ = StopAndRemoveContainer(context.Background(), target)
			return err
		}
	}

	if opt.Detach {
		if err := StartExistingContainer(ctx, target); err != nil {
			_ = StopAndRemoveContainer(context.Background(), target)
			return err
		}
		fmt.Fprintln(stdout(), target)
		return nil
	}

	if opt.Remove {
		defer func() {
			_ = StopAndRemoveContainer(context.Background(), target)
		}()
	}

	return RunWSLCInteractive(ctx, "start", "-a", "-i", target)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}
