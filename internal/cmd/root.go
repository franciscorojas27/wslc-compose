package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"wslc-compose/internal/config"
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	version             = "dev"
	composeFiles        []string
	composeFilesChanged bool
	projectName         string
	envFile             string
	profiles            []string
)

type composeFilesValue struct{}

func newComposeFilesValue() *composeFilesValue {
	return &composeFilesValue{}
}

func (v *composeFilesValue) String() string {
	if len(composeFiles) == 0 {
		return "[]"
	}
	return "[" + strings.Join(composeFiles, ",") + "]"
}

func (v *composeFilesValue) Set(val string) error {
	if !composeFilesChanged {
		composeFiles = []string{val}
		composeFilesChanged = true
	} else {
		composeFiles = append(composeFiles, val)
	}
	return nil
}

func (v *composeFilesValue) Type() string {
	return "stringArray"
}

func (v *composeFilesValue) Append(val string) error {
	composeFiles = append(composeFiles, val)
	composeFilesChanged = true
	return nil
}

func (v *composeFilesValue) Replace(val []string) error {
	if len(val) == 0 {
		composeFiles = nil
		composeFilesChanged = false
		return nil
	}
	cp := make([]string, len(val))
	copy(cp, val)
	composeFiles = cp
	composeFilesChanged = true
	return nil
}

func (v *composeFilesValue) GetSlice() []string {
	cp := make([]string, len(composeFiles))
	copy(cp, composeFiles)
	return cp
}

var rootCmd = &cobra.Command{
	Use:              "wslc-compose",
	Short:            "Container orchestrator for Windows using wslc.exe",
	Version:          version,
	TraverseChildren: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if shouldSkipWSLCLookup(cmd) {
			return nil
		}
		if _, err := runtime.ResolveWSLCBinary(); err != nil {
			return err
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

func runWithSignals(fn func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
		}
	}()

	return fn(ctx)
}

func Execute() {
	if err := runWithSignals(rootCmd.ExecuteContext); err != nil {
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
	cfg, err := config.Load("", config.LoadOptions{
		Files:            resolvedComposeFiles(),
		ProjectName:      resolvedProjectName(),
		EnvFile:          envFile,
		Profiles:         activeProfiles(),
		ExplicitServices: explicitServices,
	})
	if err != nil {
		return config.ComposeFile{}, err
	}
	return cfg.FilterByProfiles(cfg.ActiveProfiles, explicitServices), nil
}

func loadComposeAll() (config.ComposeFile, error) {
	cfg, err := config.Load("", config.LoadOptions{
		Files:       resolvedComposeFiles(),
		ProjectName: resolvedProjectName(),
		EnvFile:     envFile,
		Profiles:    activeProfiles(),
	})
	if err != nil {
		return config.ComposeFile{}, err
	}
	if len(cfg.ActiveProfiles) > 0 {
		return cfg.FilterByProfiles(cfg.ActiveProfiles, nil), nil
	}
	return cfg, nil
}

func init() {
	rootCmd.PersistentFlags().VarP(newComposeFilesValue(), "file", "f", "Compose configuration file path")
	rootCmd.PersistentFlags().StringVarP(&projectName, "project-name", "p", "", "Project name")
	rootCmd.PersistentFlags().StringVar(&envFile, "env-file", "", "Specify an alternate environment file")
	rootCmd.PersistentFlags().StringArrayVar(&profiles, "profile", nil, "Specify a profile to enable")
}
