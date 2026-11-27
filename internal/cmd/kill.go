package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var killSignal string

var killCmd = &cobra.Command{
	Use:   "kill [service...]",
	Short: "Force stop service containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Kill(cmd.Context(), cfg, args, runtime.KillOptions{
			Signal: killSignal,
		})
	},
}

func init() {
	killCmd.Flags().StringVarP(&killSignal, "signal", "s", "SIGKILL", "SIGNAL to send to the container")
	rootCmd.AddCommand(killCmd)
}
