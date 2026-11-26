package cmd

import (
	"context"
	"fmt"
	"wslc-compose/internal/config"
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var detach bool

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Crea y levanta los servicios definidos en el archivo compose",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(composeFile)
		ctx := context.Background()
		if err != nil {
			return err
		}

		if len(cfg.Networks) > 0 {
			for name, net := range cfg.Networks {
				text, err := runtime.CreateNetwork(ctx, name, net)
				if err != nil {
					return err
				}
				fmt.Println(text)
			}
		}

		return nil
	},
}

func init() {
	upCmd.Flags().BoolVarP(&detach, "detach", "d", false, "Ejecutar contenedores en segundo plano")
	rootCmd.AddCommand(upCmd)
}
