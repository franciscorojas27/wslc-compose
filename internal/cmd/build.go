package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	buildNoCache bool
	buildPull    bool
	buildArgs    []string
)

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
			Pull:       buildPull,
			BuildArgs:  buildArgs,
			ProjectDir: cfg.ComposeBaseDir(),
		})
	},
}

func init() {
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false, "Do not use cache when building the image")
	buildCmd.Flags().BoolVar(&buildPull, "pull", false, "Always attempt to pull a newer version of the image")
	buildCmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil, "Set build-time variables for services")
	rootCmd.AddCommand(buildCmd)
}
