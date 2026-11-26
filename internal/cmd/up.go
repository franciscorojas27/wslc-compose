package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	detach          bool
	upBuild         bool
	upNoBuild       bool
	upForceRecreate bool
	upNoRecreate    bool
	upRemoveOrphans bool
	upWait          bool
)

var upCmd = &cobra.Command{
	Use:   "up [service...]",
	Short: "Create and start containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Up(cmd.Context(), cfg, runtime.UpOptions{
			Detach:        detach,
			Build:         upBuild,
			NoBuild:       upNoBuild,
			ForceRecreate: upForceRecreate,
			NoRecreate:    upNoRecreate,
			RemoveOrphans: upRemoveOrphans,
			Wait:          upWait,
			Services:      args,
		})
	},
}

func init() {
	upCmd.Flags().BoolVarP(&detach, "detach", "d", true, "Detached mode: run containers in the background")
	upCmd.Flags().BoolVar(&upBuild, "build", false, "Build images before starting containers")
	upCmd.Flags().BoolVar(&upNoBuild, "no-build", false, "Do not build an image, even if it is configured")
	upCmd.Flags().BoolVar(&upForceRecreate, "force-recreate", false, "Recreate containers even if their configuration has not changed")
	upCmd.Flags().BoolVar(&upNoRecreate, "no-recreate", false, "Do not recreate containers if they already exist")
	upCmd.Flags().BoolVar(&upRemoveOrphans, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	upCmd.Flags().BoolVar(&upWait, "wait", false, "Wait for services to be running and healthy")
	rootCmd.AddCommand(upCmd)
}
