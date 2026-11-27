package cmd

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func resetFlag(fs *pflag.FlagSet, name string) {
	f := fs.Lookup(name)
	if f == nil {
		return
	}
	if sv, ok := f.Value.(pflag.SliceValue); ok {
		_ = sv.Replace(nil)
	} else {
		_ = f.Value.Set(f.DefValue)
	}
	f.Changed = false
}

func TestAuditBugGlobalShortFileFlagBrokenOnLogsAndRm(t *testing.T) {
	origFiles := append([]string(nil), composeFiles...)
	origForce := rmForce
	origFollow := logsFollow
	origPreRun := rootCmd.PersistentPreRunE
	origRmRun := rmCmd.RunE
	origLogsRun := logsCmd.RunE

	t.Cleanup(func() {
		composeFiles = origFiles
		rmForce = origForce
		logsFollow = origFollow
		rootCmd.PersistentPreRunE = origPreRun
		rmCmd.RunE = origRmRun
		logsCmd.RunE = origLogsRun
		rootCmd.SetArgs(nil)
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rmCmd.Flags(), "file")
		resetFlag(rmCmd.Flags(), "force")
		resetFlag(logsCmd.Flags(), "file")
		resetFlag(logsCmd.Flags(), "follow")
	})

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return nil
	}

	var gotArgs []string
	rmCmd.RunE = func(cmd *cobra.Command, args []string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	}
	logsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	}

	resetAll := func() {
		composeFiles = nil
		rmForce = false
		logsFollow = false
		gotArgs = nil
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rmCmd.Flags(), "file")
		resetFlag(rmCmd.Flags(), "force")
		resetFlag(logsCmd.Flags(), "file")
		resetFlag(logsCmd.Flags(), "follow")
	}

	t.Run("root -f before rm does not trigger rm --force", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "staging.yaml", "rm", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		wantFiles := []string{"staging.yaml"}
		if !reflect.DeepEqual(composeFiles, wantFiles) || rmForce || !reflect.DeepEqual(gotArgs, []string{"web"}) {
			t.Fatalf("got files=%v rmForce=%v args=%v, want files=%v rmForce=false args=[web]",
				composeFiles, rmForce, gotArgs, wantFiles)
		}
	})

	t.Run("root -f before rm allows rm -f afterwards", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "staging.yaml", "rm", "-f", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		wantFiles := []string{"staging.yaml"}
		if !reflect.DeepEqual(composeFiles, wantFiles) || !rmForce || !reflect.DeepEqual(gotArgs, []string{"web"}) {
			t.Fatalf("got files=%v rmForce=%v args=%v, want files=%v rmForce=true args=[web]",
				composeFiles, rmForce, gotArgs, wantFiles)
		}
	})

	t.Run("repeatable root -f before logs does not trigger logs --follow", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "base.yaml", "-f", "override.yaml", "logs", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		wantFiles := []string{"base.yaml", "override.yaml"}
		if !reflect.DeepEqual(composeFiles, wantFiles) || logsFollow || !reflect.DeepEqual(gotArgs, []string{"web"}) {
			t.Fatalf("got files=%v logsFollow=%v args=%v, want files=%v logsFollow=false args=[web]",
				composeFiles, logsFollow, gotArgs, wantFiles)
		}
	})

	t.Run("repeatable root -f before logs allows logs -f afterwards", func(t *testing.T) {
		resetAll()
		rootCmd.SetArgs([]string{"-f", "base.yaml", "-f", "override.yaml", "logs", "-f", "web"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		wantFiles := []string{"base.yaml", "override.yaml"}
		if !reflect.DeepEqual(composeFiles, wantFiles) || !logsFollow || !reflect.DeepEqual(gotArgs, []string{"web"}) {
			t.Fatalf("got files=%v logsFollow=%v args=%v, want files=%v logsFollow=true args=[web]",
				composeFiles, logsFollow, gotArgs, wantFiles)
		}
	})
}

func TestAuditBugPortCommandDefaultProtocolOverridesEmbeddedUDP(t *testing.T) {
	origProto := portProtocol
	t.Cleanup(func() {
		portProtocol = origProto
		resetFlag(portCmd.Flags(), "protocol")
	})

	t.Run("omitted --protocol does not force protocol filter", func(t *testing.T) {
		resetFlag(portCmd.Flags(), "protocol")
		if err := portCmd.ParseFlags([]string{"dns", "53/udp"}); err != nil {
			t.Fatalf("ParseFlags failed: %v", err)
		}
		if portCmd.Flags().Changed("protocol") {
			t.Fatalf("expected protocol flag Changed() to be false")
		}
		if got := resolvePortProtocol(portCmd); got != "" {
			t.Fatalf("expected empty protocol filter when --protocol is omitted, got %q", got)
		}
	})

	t.Run("explicit --protocol udp passes udp filter", func(t *testing.T) {
		resetFlag(portCmd.Flags(), "protocol")
		if err := portCmd.ParseFlags([]string{"--protocol", "udp", "dns", "53"}); err != nil {
			t.Fatalf("ParseFlags failed: %v", err)
		}
		if !portCmd.Flags().Changed("protocol") {
			t.Fatalf("expected protocol flag Changed() to be true")
		}
		if got := resolvePortProtocol(portCmd); got != "udp" {
			t.Fatalf("expected protocol filter 'udp', got %q", got)
		}
	})

	t.Run("explicit --protocol tcp passes tcp filter", func(t *testing.T) {
		resetFlag(portCmd.Flags(), "protocol")
		if err := portCmd.ParseFlags([]string{"--protocol", "tcp", "web", "80"}); err != nil {
			t.Fatalf("ParseFlags failed: %v", err)
		}
		if !portCmd.Flags().Changed("protocol") {
			t.Fatalf("expected protocol flag Changed() to be true")
		}
		if got := resolvePortProtocol(portCmd); got != "tcp" {
			t.Fatalf("expected protocol filter 'tcp', got %q", got)
		}
	})
}

func TestAuditBugCommandsAcceptUnexpectedPositionalArgs(t *testing.T) {
	for _, c := range []*struct {
		name string
		fn   func([]string) error
	}{
		{"down", func(a []string) error { return downCmd.ValidateArgs(a) }},
		{"ps", func(a []string) error { return psCmd.ValidateArgs(a) }},
		{"config", func(a []string) error { return configCmd.ValidateArgs(a) }},
		{"volumes", func(a []string) error { return volumesCmd.ValidateArgs(a) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn([]string{"unexpected-service"}); err == nil {
				t.Fatalf("command %q accepted unexpected positional argument instead of rejecting it", c.name)
			}
			if err := c.fn(nil); err != nil {
				t.Fatalf("command %q rejected empty args: %v", c.name, err)
			}
		})
	}
}

func TestAuditBugCobraGlobalEnvInitAndFlagStateAccumulation(t *testing.T) {
	origBuildArgs := append([]string(nil), buildArgs...)
	origFiles := append([]string(nil), composeFiles...)
	origProfiles := append([]string(nil), profiles...)
	origProject := projectName
	t.Cleanup(func() {
		buildArgs = origBuildArgs
		composeFiles = origFiles
		profiles = origProfiles
		projectName = origProject
		resetFlag(buildCmd.Flags(), "build-arg")
		resetFlag(rootCmd.PersistentFlags(), "file")
		resetFlag(rootCmd.PersistentFlags(), "profile")
		resetFlag(rootCmd.PersistentFlags(), "project-name")
	})

	resetFlag(buildCmd.Flags(), "build-arg")
	if err := buildCmd.Flags().Parse([]string{"--build-arg", "A=1"}); err != nil {
		t.Fatalf("first parse failed: %v", err)
	}
	if !reflect.DeepEqual(buildArgs, []string{"A=1"}) {
		t.Fatalf("unexpected first parse buildArgs: %v", buildArgs)
	}

	resetFlag(buildCmd.Flags(), "build-arg")
	if err := buildCmd.Flags().Parse([]string{"--build-arg", "B=2"}); err != nil {
		t.Fatalf("second parse failed: %v", err)
	}
	if !reflect.DeepEqual(buildArgs, []string{"B=2"}) {
		t.Fatalf("StringArrayVar leaked values across parses after reset: got %v", buildArgs)
	}

	composeFiles = nil
	profiles = nil
	projectName = ""
	t.Setenv("COMPOSE_FILE", "base.yaml"+string(os.PathListSeparator)+"override.yaml")
	t.Setenv("COMPOSE_PROFILES", "debug, metrics")
	t.Setenv("COMPOSE_PROJECT_NAME", "envproj")

	if got := resolvedComposeFiles(); !reflect.DeepEqual(got, []string{"base.yaml", "override.yaml"}) {
		t.Fatalf("got COMPOSE_FILE %v, want [base.yaml override.yaml]", got)
	}
	if got := activeProfiles(); !reflect.DeepEqual(got, []string{"debug", "metrics"}) {
		t.Fatalf("got COMPOSE_PROFILES %v, want [debug metrics]", got)
	}
	if got := resolvedProjectName(); got != "envproj" {
		t.Fatalf("got COMPOSE_PROJECT_NAME %q, want envproj", got)
	}

	composeFiles = []string{"cli.yaml"}
	profiles = []string{"prod"}
	projectName = "cliproj"
	if got := resolvedComposeFiles(); !reflect.DeepEqual(got, []string{"cli.yaml"}) {
		t.Fatalf("CLI -f must take precedence over COMPOSE_FILE, got %v", got)
	}
	if got := activeProfiles(); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("CLI --profile must take precedence over COMPOSE_PROFILES, got %v", got)
	}
	if got := resolvedProjectName(); got != "cliproj" {
		t.Fatalf("CLI -p must take precedence over COMPOSE_PROJECT_NAME, got %q", got)
	}
}

func TestAuditPersistentPreRunSkipsWSLCLookupForCompletionAndHelp(t *testing.T) {
	t.Setenv("PATH", "")
	rootCmd.InitDefaultCompletionCmd()
	rootCmd.InitDefaultHelpCmd()

	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run("completion_"+shell, func(t *testing.T) {
			child, _, err := rootCmd.Find([]string{"completion", shell})
			if err != nil {
				t.Fatalf("failed to find completion %s command: %v", shell, err)
			}
			if err := rootCmd.PersistentPreRunE(child, nil); err != nil {
				t.Fatalf("completion %s failed without wslc in PATH: %v", shell, err)
			}
		})
	}

	t.Run("version_command_skips_lookup", func(t *testing.T) {
		versionChild := &cobra.Command{Use: "version"}
		rootCmd.AddCommand(versionChild)
		t.Cleanup(func() {
			rootCmd.RemoveCommand(versionChild)
		})
		if err := rootCmd.PersistentPreRunE(versionChild, nil); err != nil {
			t.Fatalf("expected version to skip wslc lookup, got: %v", err)
		}
	})

	t.Run("help_skips_lookup", func(t *testing.T) {
		helpChild, _, err := rootCmd.Find([]string{"help"})
		if err != nil {
			t.Fatalf("failed to find help command: %v", err)
		}
		if err := rootCmd.PersistentPreRunE(helpChild, nil); err != nil {
			t.Fatalf("expected help to skip wslc lookup, got: %v", err)
		}
	})

	t.Run("runtime_command_requires_wslc", func(t *testing.T) {
		if err := rootCmd.PersistentPreRunE(psCmd, nil); err == nil {
			t.Fatalf("expected ps to fail when wslc is missing from PATH")
		}
	})
}

func TestRunWithSignalsCancelsContextOnExit(t *testing.T) {
	var captured context.Context
	err := runWithSignals(func(ctx context.Context) error {
		captured = ctx
		if ctx.Err() != nil {
			t.Fatalf("expected active context during execution, got %v", ctx.Err())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil || captured.Err() == nil {
		t.Fatalf("expected context to be canceled cleanly on exit")
	}
}
