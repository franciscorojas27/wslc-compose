package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var statsNoStream bool

var statsCmd = &cobra.Command{
	Use:   "stats [service...]",
	Short: "Display a live stream of container resource usage statistics",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Stats(cmd.Context(), cfg, args, runtime.StatsOptions{
			NoStream: statsNoStream,
		})
	},
}

func init() {
	statsCmd.Flags().BoolVar(&statsNoStream, "no-stream", false, "Disable streaming stats and only pull the first result")
	rootCmd.AddCommand(statsCmd)
}
