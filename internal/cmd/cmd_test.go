package cmd

import (
	"bytes"
	"testing"
)

func TestAllSubcommandsMergeFlagsAndHelp(t *testing.T) {
	t.Cleanup(func() {
		if f := rootCmd.Flags().Lookup("help"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
		if f := rootCmd.PersistentFlags().Lookup("help"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
		for _, c := range rootCmd.Commands() {
			if f := c.Flags().Lookup("help"); f != nil {
				_ = f.Value.Set("false")
				f.Changed = false
			}
		}
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	subcommands := rootCmd.Commands()
	if len(subcommands) == 0 {
		t.Fatalf("expected subcommands to be registered on rootCmd")
	}

	for _, sub := range subcommands {
		sub := sub
		t.Run(sub.Name(), func(t *testing.T) {
			t.Cleanup(func() {
				if f := sub.Flags().Lookup("help"); f != nil {
					_ = f.Value.Set("false")
					f.Changed = false
				}
				if f := rootCmd.Flags().Lookup("help"); f != nil {
					_ = f.Value.Set("false")
					f.Changed = false
				}
				if f := rootCmd.PersistentFlags().Lookup("help"); f != nil {
					_ = f.Value.Set("false")
					f.Changed = false
				}
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
				rootCmd.SetArgs(nil)
			})

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("subcommand %q panicked during flag merge or help: %v", sub.Name(), r)
				}
			}()

			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs([]string{sub.Name(), "--help"})

			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("subcommand %q --help returned error: %v", sub.Name(), err)
			}
		})
	}
}

func TestLogsAndRmShortFlagMappings(t *testing.T) {
	logsFlags := logsCmd.Flags()
	fFlag := logsFlags.ShorthandLookup("f")
	if fFlag == nil || fFlag.Name != "follow" {
		t.Fatalf("expected logs -f to map to --follow, got: %+v", fFlag)
	}
	if logsFlags.Lookup("file") == nil {
		t.Fatalf("expected logs to retain --file long flag")
	}

	rmFlags := rmCmd.Flags()
	rmFFlag := rmFlags.ShorthandLookup("f")
	if rmFFlag == nil || rmFFlag.Name != "force" {
		t.Fatalf("expected rm -f to map to --force, got: %+v", rmFFlag)
	}
	if rmFlags.Lookup("file") == nil {
		t.Fatalf("expected rm to retain --file long flag")
	}
}

func TestUpDefaultsToForeground(t *testing.T) {
	upFlags := upCmd.Flags()
	detachFlag := upFlags.Lookup("detach")
	if detachFlag == nil {
		t.Fatalf("expected up command to register --detach flag")
	}
	if detachFlag.DefValue != "false" {
		t.Fatalf("expected --detach default to be false, got %q", detachFlag.DefValue)
	}
	shortFlag := upFlags.ShorthandLookup("d")
	if shortFlag == nil || shortFlag.Name != "detach" {
		t.Fatalf("expected up -d to map to --detach, got: %+v", shortFlag)
	}

	t.Cleanup(func() {
		_ = upFlags.Set("detach", "false")
		detach = false
	})

	if detach {
		t.Fatalf("expected initial detach variable to be false")
	}

	if err := upFlags.Parse([]string{"-d"}); err != nil {
		t.Fatalf("failed to parse -d flag: %v", err)
	}
	if !detach {
		t.Fatalf("expected -d to set detach=true")
	}

	if err := upFlags.Set("detach", "false"); err != nil {
		t.Fatalf("failed to reset detach flag: %v", err)
	}
	if err := upFlags.Parse([]string{}); err != nil {
		t.Fatalf("failed to parse empty args: %v", err)
	}
	if detach {
		t.Fatalf("expected up without -d to keep detach=false")
	}
}

func TestUpAndRunNoDepsFlags(t *testing.T) {
	upFlags := upCmd.Flags()
	upNoDepsFlag := upFlags.Lookup("no-deps")
	if upNoDepsFlag == nil {
		t.Fatalf("expected up command to register --no-deps flag")
	}
	if upNoDepsFlag.DefValue != "false" {
		t.Fatalf("expected up --no-deps default to be false, got %q", upNoDepsFlag.DefValue)
	}

	runFlags := runCmd.Flags()
	runNoDepsFlag := runFlags.Lookup("no-deps")
	if runNoDepsFlag == nil {
		t.Fatalf("expected run command to register --no-deps flag")
	}
	if runNoDepsFlag.DefValue != "false" {
		t.Fatalf("expected run --no-deps default to be false, got %q", runNoDepsFlag.DefValue)
	}

	t.Cleanup(func() {
		_ = upFlags.Set("no-deps", "false")
		noDeps = false
		_ = runFlags.Set("no-deps", "false")
		runNoDeps = false
	})

	if err := upFlags.Parse([]string{"--no-deps"}); err != nil {
		t.Fatalf("failed to parse up --no-deps: %v", err)
	}
	if !noDeps {
		t.Fatalf("expected up --no-deps to set noDeps=true")
	}

	if err := runFlags.Parse([]string{"--no-deps"}); err != nil {
		t.Fatalf("failed to parse run --no-deps: %v", err)
	}
	if !runNoDeps {
		t.Fatalf("expected run --no-deps to set runNoDeps=true")
	}
}

func TestBuildPullAndBuildArgFlags(t *testing.T) {
	flags := buildCmd.Flags()
	if f := flags.Lookup("pull"); f == nil || f.DefValue != "false" {
		t.Fatalf("expected build --pull flag defaulting to false, got: %+v", f)
	}
	if f := flags.Lookup("build-arg"); f == nil {
		t.Fatalf("expected build --build-arg flag to be registered")
	}

	t.Cleanup(func() {
		_ = flags.Set("pull", "false")
		buildPull = false
		buildArgs = nil
	})

	if err := flags.Parse([]string{"--pull", "--build-arg", "A=1", "--build-arg", "B=2"}); err != nil {
		t.Fatalf("failed to parse build flags: %v", err)
	}
	if !buildPull {
		t.Fatalf("expected --pull to set buildPull=true")
	}
	if len(buildArgs) != 2 || buildArgs[0] != "A=1" || buildArgs[1] != "B=2" {
		t.Fatalf("unexpected buildArgs: %v", buildArgs)
	}
}

func TestDailyDriverCommandFlagsAndUnsupportedOmitted(t *testing.T) {
	if f := cpCmd.Flags().ShorthandLookup("a"); f == nil || f.Name != "archive" {
		t.Fatalf("expected cp -a to map to --archive, got: %+v", f)
	}
	if f := cpCmd.Flags().ShorthandLookup("L"); f == nil || f.Name != "follow-link" {
		t.Fatalf("expected cp -L to map to --follow-link, got: %+v", f)
	}
	if f := portCmd.Flags().Lookup("protocol"); f == nil || f.DefValue != "tcp" {
		t.Fatalf("expected port --protocol defaulting to tcp, got: %+v", f)
	}
	if !portCmd.HasAlias("ports") {
		t.Fatalf("expected port command to expose 'ports' alias")
	}
	foundPort, _, err := rootCmd.Find([]string{"ports"})
	if err != nil || foundPort != portCmd {
		t.Fatalf("expected 'ports' alias to resolve to portCmd, got cmd=%v err=%v", foundPort, err)
	}
	if f := killCmd.Flags().ShorthandLookup("s"); f == nil || f.Name != "signal" || f.DefValue != "SIGKILL" {
		t.Fatalf("expected kill -s/--signal defaulting to SIGKILL, got: %+v", f)
	}
	if f := statsCmd.Flags().Lookup("no-stream"); f == nil || f.DefValue != "false" {
		t.Fatalf("expected stats --no-stream defaulting to false, got: %+v", f)
	}
	if f := eventsCmd.Flags().Lookup("json"); f == nil || f.DefValue != "false" {
		t.Fatalf("expected events --json defaulting to false, got: %+v", f)
	}

	forbidden := map[string]bool{"pause": true, "unpause": true, "top": true}
	for _, sub := range rootCmd.Commands() {
		if forbidden[sub.Name()] {
			t.Fatalf("unsupported command %q must not be registered", sub.Name())
		}
	}
}
