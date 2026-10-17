package tunnelserver

import (
	"context"
	"testing"

	"github.com/devsy-org/devsy/pkg/agent/tunnel"
	"github.com/stretchr/testify/require"
)

func TestGitHubCredentialsRequiresIndependentPermission(t *testing.T) {
	for _, server := range []*tunnelServer{
		New(),
		New(WithAllowGitCredentials(true), WithAllowDockerCredentials(true)),
		New(WithAllowGitHubCredentials(false)),
	} {
		response, err := server.GitHubCredentials(context.Background(), &tunnel.Empty{})
		require.ErrorContains(t, err, "forbidden")
		require.Nil(t, response)
	}
}
