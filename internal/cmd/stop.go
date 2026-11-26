package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var stopTimeout int

var stopCmd = &cobra.Command{
	Use:   "stop [service...]",
	Short: "Stop services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Stop(cmd.Context(), cfg, args, runtime.StopOptions{
			Timeout: stopTimeout,
		})
	},
}

func init() {
	stopCmd.Flags().IntVarP(&stopTimeout, "timeout", "t", -1, "Specify a shutdown timeout in seconds")
	rootCmd.AddCommand(stopCmd)
}
