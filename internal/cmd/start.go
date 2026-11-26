package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start [service...]",
	Short: "Start services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Start(cmd.Context(), cfg, args)
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
