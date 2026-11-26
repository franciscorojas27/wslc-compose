package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	configServices bool
	configVolumes  bool
	configQuiet    bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Parse, resolve and render Compose file in canonical format",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(nil)
		if err != nil {
			return err
		}
		return runtime.Config(cfg, runtime.ConfigOptions{
			Services: configServices,
			Volumes:  configVolumes,
			Quiet:    configQuiet,
		})
	},
}

func init() {
	configCmd.Flags().BoolVar(&configServices, "services", false, "Print the service names, one per line")
	configCmd.Flags().BoolVar(&configVolumes, "volumes", false, "Print the volume names, one per line")
	configCmd.Flags().BoolVarP(&configQuiet, "quiet", "q", false, "Only validate the configuration, do not print anything")
	rootCmd.AddCommand(configCmd)
}
