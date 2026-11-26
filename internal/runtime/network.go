package runtime

import (
	"context"
	"errors"
	"fmt"
	"wslc-compose/internal/config"
)

func CreateNetwork(ctx context.Context, name string, net config.Network) (string, error) {
	if net.External {
		return "", nil
	}

	args := []string{"network", "create"}

	if net.Driver != "" && net.Driver != "nat" {
		args = append(args, "--driver", net.Driver)
	}

	for k, v := range net.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	args = append(args, name)

	res, err := RunWSLC(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return name, nil
		}
		return "", fmt.Errorf("Resource %s was not created: %w", name, err)
	}
	return res, nil
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
		return "", fmt.Errorf("Resource %s was not deleted: %w", name, err)
	}
	return res, nil
}
