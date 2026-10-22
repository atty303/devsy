package container

import (
	"github.com/devsy-org/devsy/cmd/flags"
	"github.com/devsy-org/devsy/pkg/envfile"
	"github.com/spf13/cobra"
)

func NewContainerCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "container",
		Short: "Inspect the current container",
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			envfile.Apply()
		},
	}
	cmd.AddCommand(newInfoCmd(globalFlags))
	return cmd
}
