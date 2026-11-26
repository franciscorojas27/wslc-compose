package cmd

import (
	"os"
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	logsFollow     bool
	logsTail       string
	logsTimestamps bool
)

var logsCmd = &cobra.Command{
	Use:   "logs [service...]",
	Short: "View output from containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Logs(cmd.Context(), cfg, args, runtime.LogsOptions{
			Follow:     logsFollow,
			Tail:       logsTail,
			Timestamps: logsTimestamps,
		})
	},
}

func init() {
	logsCmd.Flags().StringVar(&composeFile, "file", os.Getenv("COMPOSE_FILE"), "Compose configuration file")
	_ = logsCmd.Flags().MarkHidden("file")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().StringVar(&logsTail, "tail", "all", "Number of lines to show from the end of the logs for each container")
	logsCmd.Flags().BoolVarP(&logsTimestamps, "timestamps", "t", false, "Show timestamps")
	rootCmd.AddCommand(logsCmd)
}
