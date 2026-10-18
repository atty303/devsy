package cmdinternal

import (
	"os"

	"github.com/devsy-org/devsy/pkg/config"
	"github.com/devsy-org/devsy/pkg/ghcredentials"
	"github.com/devsy-org/devsy/pkg/log"
	"github.com/spf13/cobra"
)

func NewGHCmd() *cobra.Command {
	return &cobra.Command{
		Use: "gh", Hidden: true,
		DisableFlagParsing: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			log.Init(
				log.Config{Level: os.Getenv(config.EnvLogLevel), DefaultLevel: log.DefaultLevel},
			)
			return nil
		},
		Run: func(_ *cobra.Command, args []string) { os.Exit(ghcredentials.Run(args)) },
	}
}
