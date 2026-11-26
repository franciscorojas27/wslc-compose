package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"wslc-compose/internal/config"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	composeFile string
	projectName string
	envFile     string
	profiles    []string
)

var rootCmd = &cobra.Command{
	Use:     "wslc-compose",
	Short:   "Container orchestrator for Windows using wslc.exe",
	Version: version,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		switch cmd.Name() {
		case "config", "help", "completion", "__complete":
			return nil
		}
		if _, err := exec.LookPath("wslc"); err != nil {
			return fmt.Errorf("'wslc' was not found in system PATH: %w", err)
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func activeProfiles() []string {
	var list []string
	if envProfiles := os.Getenv("COMPOSE_PROFILES"); envProfiles != "" {
		list = append(list, envProfiles)
	}
	list = append(list, profiles...)
	return list
}

func loadCompose(explicitServices []string) (config.ComposeFile, error) {
	active := activeProfiles()
	cfg, err := config.Load(composeFile, config.LoadOptions{
		ProjectName: projectName,
		EnvFile:     envFile,
		Profiles:    active,
	})
	if err != nil {
		return config.ComposeFile{}, err
	}
	return cfg.FilterByProfiles(active, explicitServices), nil
}

func loadComposeAll() (config.ComposeFile, error) {
	active := activeProfiles()
	cfg, err := config.Load(composeFile, config.LoadOptions{
		ProjectName: projectName,
		EnvFile:     envFile,
		Profiles:    active,
	})
	if err != nil {
		return config.ComposeFile{}, err
	}
	if len(active) > 0 {
		return cfg.FilterByProfiles(active, nil), nil
	}
	return cfg, nil
}

func init() {
	defaultFile := os.Getenv("COMPOSE_FILE")
	defaultProject := os.Getenv("COMPOSE_PROJECT_NAME")
	rootCmd.PersistentFlags().StringVarP(&composeFile, "file", "f", defaultFile, "Compose configuration file path")
	rootCmd.PersistentFlags().StringVarP(&projectName, "project-name", "p", defaultProject, "Project name")
	rootCmd.PersistentFlags().StringVar(&envFile, "env-file", "", "Specify an alternate environment file")
	rootCmd.PersistentFlags().StringArrayVar(&profiles, "profile", nil, "Specify a profile to enable")
}
