package cmdinternal

import (
	"github.com/devsy-org/devsy/cmd/flags"
	"github.com/devsy-org/devsy/cmd/internal/agentcontainer"
	"github.com/spf13/cobra"
)

func NewContainerCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "container",
		Short:             "Inspect the current container",
		PersistentPreRunE: agentPreRunE(globalFlags),
	}
	cmd.AddCommand(agentcontainer.NewInfoCmd(globalFlags))
	return cmd
}
