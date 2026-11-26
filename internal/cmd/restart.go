package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var restartTimeout int

var restartCmd = &cobra.Command{
	Use:   "restart [service...]",
	Short: "Restart service containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Restart(cmd.Context(), cfg, args, runtime.StopOptions{
			Timeout: restartTimeout,
		})
	},
}

func init() {
	restartCmd.Flags().IntVarP(&restartTimeout, "timeout", "t", -1, "Specify a shutdown timeout in seconds")
	rootCmd.AddCommand(restartCmd)
}
