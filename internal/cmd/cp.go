package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	cpArchive    bool
	cpFollowLink bool
)

var cpCmd = &cobra.Command{
	Use:   "cp [options] <src> <dest>",
	Short: "Copy files or folders between a service container and the local filesystem",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		explicit := runtime.ExtractCPServices(args[0], args[1])
		cfg, err := loadCompose(explicit)
		if err != nil {
			return err
		}
		return runtime.CP(cmd.Context(), cfg, args[0], args[1], runtime.CPOptions{
			Archive:    cpArchive,
			FollowLink: cpFollowLink,
		})
	},
}

func init() {
	cpCmd.Flags().BoolVarP(&cpArchive, "archive", "a", false, "Archive mode (copy all uid/gid information)")
	cpCmd.Flags().BoolVarP(&cpFollowLink, "follow-link", "L", false, "Always follow symbol link in SRC_PATH")
	rootCmd.AddCommand(cpCmd)
}
