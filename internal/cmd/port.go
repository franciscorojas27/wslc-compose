package cmd

import (
	"wslc-compose/internal/runtime"

	"github.com/spf13/cobra"
)

var portProtocol string

var portCmd = &cobra.Command{
	Use:     "port [options] <service> <private_port>",
	Aliases: []string{"ports"},
	Short:   "Print the public port for a port binding",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		service := args[0]
		privatePort := args[1]
		cfg, err := loadCompose([]string{service})
		if err != nil {
			return err
		}
		return runtime.Port(cmd.Context(), cfg, service, privatePort, runtime.PortOptions{
			Protocol: resolvePortProtocol(cmd),
		})
	},
}

func resolvePortProtocol(cmd *cobra.Command) string {
	if cmd != nil && cmd.Flags().Changed("protocol") {
		return portProtocol
	}
	return ""
}

func init() {
	portCmd.Flags().StringVar(&portProtocol, "protocol", "tcp", "Protocol of the port (tcp or udp)")
	rootCmd.AddCommand(portCmd)
}
