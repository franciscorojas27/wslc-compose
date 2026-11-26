package cmd

import (
	"bytes"
	"testing"
)

func TestAllSubcommandsMergeFlagsAndHelp(t *testing.T) {
	subcommands := rootCmd.Commands()
	if len(subcommands) == 0 {
		t.Fatalf("expected subcommands to be registered on rootCmd")
	}

	for _, sub := range subcommands {
		t.Run(sub.Name(), func(t *testing.T) {
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
