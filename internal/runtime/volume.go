package runtime

import (
	"context"
	"errors"
	"fmt"
	"wslc-compose/internal/config"
)

func CreateVolume(ctx context.Context, name string, vol config.Volume) (string, error) {
	if vol.External {
		return name, nil
	}

	args := []string{"volume", "create"}

	if vol.Driver != "" && vol.Driver != "local" {
		args = append(args, "--driver", vol.Driver)
	}

	for k, v := range vol.Labels {
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

func RemoveVolume(ctx context.Context, name string, vol config.Volume) (string, error) {
	if vol.External {
		return name, nil
	}

	res, err := RunWSLC(ctx, "volume", "rm", name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("Resource %s was not deleted: %w", name, err)
	}

	return res, nil
}
