package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Detiene y remueve los servicios definidos en el archivo compose",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Deteniendo proyecto")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(downCmd)
}
