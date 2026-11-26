package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var buildNoCache bool

var buildCmd = &cobra.Command{
	Use:   "build [service...]",
	Short: "Build or rebuild services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Build(cmd.Context(), cfg, args, runtime.BuildOptions{
			NoCache:    buildNoCache,
			ProjectDir: cfg.ProjectDir,
		})
	},
}

func init() {
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false, "Do not use cache when building the image")
	rootCmd.AddCommand(buildCmd)
}
