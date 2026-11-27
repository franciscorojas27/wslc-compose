package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var eventsJSON bool

var eventsCmd = &cobra.Command{
	Use:   "events [service...]",
	Short: "Receive real-time events from containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Events(cmd.Context(), cfg, args, runtime.EventsOptions{
			JSON: eventsJSON,
		})
	},
}

func init() {
	eventsCmd.Flags().BoolVar(&eventsJSON, "json", false, "Output events as a stream of JSON objects")
	rootCmd.AddCommand(eventsCmd)
}
