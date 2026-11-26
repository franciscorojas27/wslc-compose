package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	runRemove     bool
	runEnv        []string
	runWorkdir    string
	runUser       string
	runEntrypoint string
	runNoDeps     bool
	runDetach     bool
)

var runCmd = &cobra.Command{
	Use:   "run [options] <service> [command] [args...]",
	Short: "Run a one-off command on a service",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		service := args[0]
		command := args[1:]
		cfg, err := loadCompose([]string{service})
		if err != nil {
			return err
		}
		return runtime.Run(cmd.Context(), cfg, service, command, runtime.RunOptions{
			Remove:        runRemove,
			Env:           runEnv,
			Workdir:       runWorkdir,
			User:          runUser,
			Entrypoint:    runEntrypoint,
			HasEntrypoint: cmd.Flags().Changed("entrypoint"),
			NoDeps:        runNoDeps,
			Detach:        runDetach,
		})
	},
}

func init() {
	runCmd.Flags().SetInterspersed(false)
	runCmd.Flags().BoolVar(&runRemove, "rm", false, "Automatically remove the container when it exits")
	runCmd.Flags().StringArrayVarP(&runEnv, "env", "e", nil, "Set environment variables")
	runCmd.Flags().StringVarP(&runWorkdir, "workdir", "w", "", "Working directory inside the container")
	runCmd.Flags().StringVarP(&runUser, "user", "u", "", "Run as specified username or uid")
	runCmd.Flags().StringVar(&runEntrypoint, "entrypoint", "", "Override the entrypoint of the image")
	runCmd.Flags().BoolVar(&runNoDeps, "no-deps", false, "Do not start linked services")
	runCmd.Flags().BoolVarP(&runDetach, "detach", "d", false, "Run container in background and print container ID")
	rootCmd.AddCommand(runCmd)
}
