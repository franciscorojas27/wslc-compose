package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"wslc-compose/internal/config"

	"github.com/spf13/cobra"
)

var (
	version      = "dev"
	composeFiles []string
	projectName  string
	envFile      string
	profiles     []string
)

var rootCmd = &cobra.Command{
	Use:              "wslc-compose",
	Short:            "Container orchestrator for Windows using wslc.exe",
	Version:          version,
	TraverseChildren: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if shouldSkipWSLCLookup(cmd) {
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

func shouldSkipWSLCLookup(cmd *cobra.Command) bool {
	for cur := cmd; cur != nil; cur = cur.Parent() {
		switch cur.Name() {
		case "config", "help", "version", "completion", "__complete", "__completeNoDesc":
			return true
		}
	}
	return false
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

func resolvedComposeFiles() []string {
	if len(composeFiles) > 0 {
		out := make([]string, 0, len(composeFiles))
		for _, f := range composeFiles {
			trimmed := strings.TrimSpace(f)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	envFiles := os.Getenv("COMPOSE_FILE")
	if envFiles == "" {
		return nil
	}
	parts := strings.Split(envFiles, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func resolvedProjectName() string {
	if projectName != "" {
		return projectName
	}
	return os.Getenv("COMPOSE_PROJECT_NAME")
}

func activeProfiles() []string {
	if len(profiles) > 0 {
		out := make([]string, 0, len(profiles))
		for _, p := range profiles {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	envProfiles := os.Getenv("COMPOSE_PROFILES")
	if envProfiles == "" {
		return nil
	}
	parts := strings.Split(envProfiles, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func loadCompose(explicitServices []string) (config.ComposeFile, error) {
	active := activeProfiles()
	cfg, err := config.Load("", config.LoadOptions{
		Files:       resolvedComposeFiles(),
		ProjectName: resolvedProjectName(),
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
	cfg, err := config.Load("", config.LoadOptions{
		Files:       resolvedComposeFiles(),
		ProjectName: resolvedProjectName(),
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
	rootCmd.PersistentFlags().StringArrayVarP(&composeFiles, "file", "f", nil, "Compose configuration file path")
	rootCmd.PersistentFlags().StringVarP(&projectName, "project-name", "p", "", "Project name")
	rootCmd.PersistentFlags().StringVar(&envFile, "env-file", "", "Specify an alternate environment file")
	rootCmd.PersistentFlags().StringArrayVar(&profiles, "profile", nil, "Specify a profile to enable")
}
