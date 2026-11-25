package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Crea y levanta los servicios definidos en el archivo compose",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Levantando proyecto")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(upCmd)
}
