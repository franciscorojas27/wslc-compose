package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var volumesCmd = &cobra.Command{
	Use:   "volumes",
	Short: "List volumes declared in the Compose file",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadComposeAll()
		if err != nil {
			return err
		}
		return runtime.Volumes(cmd.Context(), cfg)
	},
}

func init() {
	rootCmd.AddCommand(volumesCmd)
}
