package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	psQuiet    bool
	psAll      bool
	psServices bool
	psStatus   []string
	psFormat   string
)

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List containers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadComposeAll()
		if err != nil {
			return err
		}
		return runtime.PS(cmd.Context(), cfg, runtime.PSOptions{
			Quiet:    psQuiet,
			All:      psAll,
			Services: psServices,
			Status:   psStatus,
			Format:   psFormat,
		})
	},
}

func init() {
	psCmd.Flags().BoolVarP(&psQuiet, "quiet", "q", false, "Only display container names or IDs")
	psCmd.Flags().BoolVarP(&psAll, "all", "a", false, "Show all stopped containers")
	psCmd.Flags().BoolVar(&psServices, "services", false, "Display services")
	psCmd.Flags().StringArrayVar(&psStatus, "status", nil, "Filter services by status (running, exited, paused, restarting, created, dead)")
	psCmd.Flags().StringVar(&psFormat, "format", "table", "Format output (\"table\" or \"json\")")
	rootCmd.AddCommand(psCmd)
}
