package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"wslc-compose/internal/config"
)

func CreateNetwork(ctx context.Context, name string, net config.Network) (string, error) {
	res, _, err := createNetwork(ctx, name, net)
	return res, err
}

func createNetwork(ctx context.Context, name string, net config.Network) (string, bool, error) {
	if net.External {
		return "", false, nil
	}

	args := []string{"network", "create"}

	if net.Driver != "" && net.Driver != "nat" {
		args = append(args, "--driver", net.Driver)
	}

	if net.Internal {
		args = append(args, "--internal")
	}

	if net.IPAM.Driver != "" {
		args = append(args, "--ipam-driver", net.IPAM.Driver)
	}

	for _, cfg := range net.IPAM.Config {
		if cfg.Subnet != "" {
			args = append(args, "--subnet", cfg.Subnet)
		}
		if cfg.Gateway != "" {
			args = append(args, "--gateway", cfg.Gateway)
		}
	}

	if len(net.Labels) > 0 {
		keys := make([]string, 0, len(net.Labels))
		for k := range net.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--label", fmt.Sprintf("%s=%s", k, net.Labels[k]))
		}
	}

	args = append(args, name)

	res, err := RunWSLC(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return name, false, nil
		}
		return "", false, fmt.Errorf("failed to create network %q: %w", name, err)
	}
	return res, true, nil
}

func RemoveNetwork(ctx context.Context, name string, net config.Network) (string, error) {
	if net.External {
		return "", nil
	}

	res, err := RunWSLC(ctx, "network", "rm", name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to remove network %q: %w", name, err)
	}
	return res, nil
}

type NetworkAttachment struct {
	Network string
	Aliases []string
	IPv4    string
}

func buildNetworkConnectArgs(att NetworkAttachment, containerRef string) []string {
	args := []string{"network", "connect"}
	if att.IPv4 != "" {
		args = append(args, "--ip", att.IPv4)
	}
	for _, alias := range att.Aliases {
		if alias != "" {
			args = append(args, "--network-alias", alias)
		}
	}
	args = append(args, att.Network, containerRef)
	return args
}

func ConnectNetwork(ctx context.Context, att NetworkAttachment, containerRef string) error {
	args := buildNetworkConnectArgs(att, containerRef)
	if _, err := RunWSLC(ctx, args...); err != nil {
		return fmt.Errorf("failed to connect container %q to network %q: %w", containerRef, att.Network, err)
	}
	return nil
}
