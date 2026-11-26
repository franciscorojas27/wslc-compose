package cmd

import (
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

		for name, vol := range cfg.Volumes {
			fmt.Printf("Volumen ' %s ' en creacion \n", name)
			fmt.Printf("   External ' %t ' \n", vol.External)
			if vol.Driver != "" {
				fmt.Printf("   Driver ' %s ' \n", vol.Driver)
			}
			if len(vol.Labels) > 0 {
				for name, label := range vol.Labels {
					fmt.Printf("   Label ' %s = %s' en creacion \n", name, label)
				}
			}

		}

		return nil
	},
}

func init() {
	upCmd.Flags().BoolVarP(&detach, "detach", "d", false, "Ejecutar contenedores en segundo plano")
	rootCmd.AddCommand(upCmd)
}
