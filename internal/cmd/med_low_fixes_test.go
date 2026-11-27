package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
)

func TestGlobalShortFilePreservedWithLocalLongFileOnRmAndLogs(t *testing.T) {
	origFiles := append([]string(nil), composeFiles...)
	origChanged := composeFilesChanged
	origPreRun := rootCmd.PersistentPreRunE
	origRmRun := rmCmd.RunE
	origLogsRun := logsCmd.RunE

	t.Cleanup(func() {
		composeFiles = origFiles
		composeFilesChanged = origChanged
		rootCmd.PersistentPreRunE = origPreRun
		rmCmd.RunE = origRmRun
		logsCmd.RunE = origLogsRun
		rootCmd.SetArgs(nil)
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rmCmd.Flags(), "file")
		resetFlag(logsCmd.Flags(), "file")
	})

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return nil
	}

	var gotResolved []string
	rmCmd.RunE = func(cmd *cobra.Command, args []string) error {
		gotResolved = resolvedComposeFiles()
		return nil
	}
	logsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		gotResolved = resolvedComposeFiles()
		return nil
	}

	resetAll := func() {
		gotResolved = nil
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rmCmd.Flags(), "file")
		resetFlag(logsCmd.Flags(), "file")
	}

	t.Run("rm combines global -f and local --file", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "base.yaml", "rm", "--file", "override.yaml", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		want := []string{"base.yaml", "override.yaml"}
		if !reflect.DeepEqual(gotResolved, want) {
			t.Fatalf("got %v, want %v", gotResolved, want)
		}
	})

	t.Run("logs combines global -f and local --file", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "base.yaml", "logs", "--file", "override.yaml", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		want := []string{"base.yaml", "override.yaml"}
		if !reflect.DeepEqual(gotResolved, want) {
			t.Fatalf("got %v, want %v", gotResolved, want)
		}
	})
}

func TestLoadComposeAllPreservesActiveProfilesState(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	content := "services:\n  web:\n    image: nginx:alpine\n  debug:\n    image: busybox:latest\n    profiles:\n      - debug\n"
	if err := os.WriteFile(composePath, []byte(content), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	origFiles := append([]string(nil), composeFiles...)
	origChanged := composeFilesChanged
	origProfiles := append([]string(nil), profiles...)
	t.Cleanup(func() {
		composeFiles = origFiles
		composeFilesChanged = origChanged
		profiles = origProfiles
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rootCmd.PersistentFlags(), "profile")
	})

	composeFiles = []string{composePath}
	composeFilesChanged = true
	profiles = nil
	t.Setenv("COMPOSE_PROFILES", "")

	cfgNoProf, err := loadComposeAll()
	if err != nil {
		t.Fatalf("loadComposeAll failed: %v", err)
	}
	if len(cfgNoProf.ActiveProfiles) != 0 {
		t.Fatalf("expected no active profiles, got %v", cfgNoProf.ActiveProfiles)
	}
	if _, ok := cfgNoProf.Services["debug"]; !ok {
		t.Fatalf("expected loadComposeAll without profiles to retain all services for container cleanup")
	}

	profiles = []string{"debug"}
	cfgWithProf, err := loadComposeAll()
	if err != nil {
		t.Fatalf("loadComposeAll with profile failed: %v", err)
	}
	if !reflect.DeepEqual(cfgWithProf.ActiveProfiles, []string{"debug"}) {
		t.Fatalf("expected ActiveProfiles=[debug], got %v", cfgWithProf.ActiveProfiles)
	}
}

func TestPersistentPreRunUsesConsistentWSLCResolution(t *testing.T) {
	binDir := t.TempDir()
	name := "wslc"
	data := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		name = "wslc.exe"
		data = []byte("MZ")
	}
	fullPath := filepath.Join(binDir, name)
	if err := os.WriteFile(fullPath, data, 0755); err != nil {
		t.Fatalf("write fake binary failed: %v", err)
	}

	t.Setenv("PATH", binDir)
	if err := rootCmd.PersistentPreRunE(psCmd, nil); err != nil {
		t.Fatalf("expected PersistentPreRunE to find %s in PATH, got: %v", name, err)
	}
}
