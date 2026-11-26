package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	downVolumes       bool
	downRemoveOrphans bool
	downTimeout       int
	downRMI           string
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop and remove containers, networks",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadComposeAll()
		if err != nil {
			return err
		}
		return runtime.Down(cmd.Context(), cfg, runtime.DownOptions{
			RemoveVolumes: downVolumes,
			RemoveOrphans: downRemoveOrphans,
			Timeout:       downTimeout,
			RMI:           downRMI,
		})
	},
}

func init() {
	downCmd.Flags().BoolVarP(&downVolumes, "volumes", "v", false, "Remove named volumes declared in the Compose file")
	downCmd.Flags().BoolVar(&downRemoveOrphans, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	downCmd.Flags().IntVarP(&downTimeout, "timeout", "t", -1, "Specify a shutdown timeout in seconds")
	downCmd.Flags().StringVar(&downRMI, "rmi", "", "Remove images used by services (\"local\" or \"all\")")
	rootCmd.AddCommand(downCmd)
}
