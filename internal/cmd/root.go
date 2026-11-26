package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	composeFile string
)

var rootCmd = &cobra.Command{
	Use:          "wslc-compose",
	Short:        "Orquestador de contenedores Windows usando wslc.exe",
	Version:      version,
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
