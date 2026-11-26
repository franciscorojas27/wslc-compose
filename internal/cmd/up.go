package cmd

import (
	"encoding/json"
	"fmt"
	"wslc-compose/internal/config"

	"github.com/spf13/cobra"
)

var detach bool

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Crea y levanta los servicios definidos en el archivo compose",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(composeFile)
		if err != nil {
			return err
		}

		dump, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}

		fmt.Printf("Estructura completa cargada (detach=%v):\n%s\n", detach, string(dump))
		return nil
	},
}

func init() {
	upCmd.Flags().BoolVarP(&detach, "detach", "d", false, "Ejecutar contenedores en segundo plano")
	rootCmd.AddCommand(upCmd)
}
