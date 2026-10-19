package cmdinternal

import (
	"github.com/devsy-org/devsy/cmd/flags"
	"github.com/spf13/cobra"
)

func NewContainerCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "container",
		Short:             "Inspect the current container",
		PersistentPreRunE: agentPreRunE(globalFlags),
	}
	cmd.AddCommand(newContainerInfoCmd(globalFlags))
	return cmd
}
