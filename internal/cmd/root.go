package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	composeFile string
)

var rootCmd = &cobra.Command{
	Use:     "wslc-compose",
	Short:   "Orquestador de contenedores Windows usando wslc.exe",
	Version: version,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if _, err := exec.LookPath("wslc"); err != nil {
			return fmt.Errorf("no se encontró 'wslc' en el PATH del sistema: %w", err)
		}
		return nil
	},
	SilenceUsage: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {

	defaultFile := os.Getenv("COMPOSE_FILE")
	rootCmd.PersistentFlags().StringVarP(&composeFile, "file", "f", defaultFile, "Path to the compose file")
}
