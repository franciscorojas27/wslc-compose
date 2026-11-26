package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	pullIgnoreFailures bool
	pullQuiet          bool
)

var pullCmd = &cobra.Command{
	Use:   "pull [service...]",
	Short: "Pull service images",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Pull(cmd.Context(), cfg, args, runtime.PullOptions{
			IgnorePullFailures: pullIgnoreFailures,
			Quiet:              pullQuiet,
		})
	},
}

func init() {
	pullCmd.Flags().BoolVar(&pullIgnoreFailures, "ignore-pull-failures", false, "Pull what it can and ignore images with pull failures")
	pullCmd.Flags().BoolVarP(&pullQuiet, "quiet", "q", false, "Pull without printing progress information")
	rootCmd.AddCommand(pullCmd)
}
