package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var (
	execNoTTY   bool
	execUser    string
	execWorkdir string
	execEnv     []string
)

var execCmd = &cobra.Command{
	Use:   "exec [options] <service> <command> [args...]",
	Short: "Execute a command in a running container",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		service := args[0]
		command := args[1:]
		cfg, err := loadCompose([]string{service})
		if err != nil {
			return err
		}
		return runtime.Exec(cmd.Context(), cfg, service, command, runtime.ExecOptions{
			NoTTY:   execNoTTY,
			User:    execUser,
			Workdir: execWorkdir,
			Env:     execEnv,
		})
	},
}

func init() {
	execCmd.Flags().SetInterspersed(false)
	execCmd.Flags().BoolVarP(&execNoTTY, "no-TTY", "T", false, "Disable pseudo-TTY allocation")
	execCmd.Flags().StringVarP(&execUser, "user", "u", "", "Run the command as this user")
	execCmd.Flags().StringVarP(&execWorkdir, "workdir", "w", "", "Path to workdir directory for this command")
	execCmd.Flags().StringArrayVarP(&execEnv, "env", "e", nil, "Set environment variables")
	rootCmd.AddCommand(execCmd)
}
