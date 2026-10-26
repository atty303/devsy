package config

import (
	"testing"

	"github.com/devsy-org/devsy/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestForkReleaseAgentDefault(t *testing.T) {
	previousBase, previousDev := forkAgentDownloadBaseURL, version.DevVersion
	t.Cleanup(func() {
		forkAgentDownloadBaseURL, version.DevVersion = previousBase, previousDev
	})
	version.DevVersion = "test-development-version"
	forkAgentDownloadBaseURL = "https://github.com/atty303/devsy/releases/download/"
	t.Setenv(EnvAgentURL, "")
	require.Equal(t, forkAgentDownloadBaseURL+version.GetVersion(), DefaultAgentDownloadURL())
	t.Setenv(EnvAgentURL, "https://example.invalid/custom///")
	require.Equal(t, "https://example.invalid/custom", DefaultAgentDownloadURL())
	t.Setenv(EnvAgentURL, "")
	forkAgentDownloadBaseURL = ""
	require.Equal(t, AgentDownloadBaseURL+version.GetVersion(), DefaultAgentDownloadURL())
	version.DevVersion = version.GetVersion()
	require.Equal(t, AgentLatestDownloadURL, DefaultAgentDownloadURL())
}
