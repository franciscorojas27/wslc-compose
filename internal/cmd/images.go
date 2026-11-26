package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var imagesQuiet bool

var imagesCmd = &cobra.Command{
	Use:   "images [service...]",
	Short: "List images used by the created containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadCompose(args)
		if err != nil {
			return err
		}
		return runtime.Images(cmd.Context(), cfg, args, runtime.ImagesOptions{
			Quiet: imagesQuiet,
		})
	},
}

func init() {
	imagesCmd.Flags().BoolVarP(&imagesQuiet, "quiet", "q", false, "Only display IDs")
	rootCmd.AddCommand(imagesCmd)
}
