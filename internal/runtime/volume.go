package runtime

import (
	"context"
	"fmt"
	"wslc-compose/internal/config"
)

func CreateVolume(ctx context.Context, name string, vol config.Volume) error {
	if vol.External {
		return nil
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
		return err
	}
	if res != "" {
		fmt.Println(res)
	}
	return nil
}

func RemoveVolume(ctx context.Context, name string, vol config.Volume) error {
	if vol.External {
		return nil
	}

	res, err := RunWSLC(ctx, "volume", "rm", name)
	if err != nil {
		return err
	}
	if res != "" {
		fmt.Println(res)
	}
	return nil
}
