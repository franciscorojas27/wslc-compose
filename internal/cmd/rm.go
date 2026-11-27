package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	rmForce   bool
	rmStop    bool
	rmVolumes bool
)

var rmCmd = &cobra.Command{
	Use:   "rm [service...]",
	Short: "Remove stopped service containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Rm(cmd.Context(), cfg, args, runtime.RmOptions{
			Force:   rmForce,
			Stop:    rmStop,
			Volumes: rmVolumes,
		})
	},
}

func init() {
	rmCmd.Flags().StringArrayVar(&composeFiles, "file", nil, "Compose configuration file")
	_ = rmCmd.Flags().MarkHidden("file")
	rmCmd.Flags().BoolVarP(&rmForce, "force", "f", false, "Do not ask to confirm removal")
	rmCmd.Flags().BoolVarP(&rmStop, "stop", "s", false, "Stop the containers, if required, before removing")
	rmCmd.Flags().BoolVarP(&rmVolumes, "volumes", "v", false, "Remove any anonymous volumes attached to containers")
	rootCmd.AddCommand(rmCmd)
}
