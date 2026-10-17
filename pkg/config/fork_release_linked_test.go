//go:build forkrelease

package config

import (
	"os"
	"testing"

	"github.com/devsy-org/devsy/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestForkReleaseLinkedDefaults(t *testing.T) {
	expected := os.Getenv("DEVSY_FORK_TEST_VERSION")
	require.NotEmpty(t, expected)
	t.Setenv(EnvAgentURL, "")
	require.Equal(t, expected, version.GetVersion())
	require.Equal(
		t,
		"https://github.com/atty303/devsy/releases/download/"+expected,
		DefaultAgentDownloadURL(),
	)
}
