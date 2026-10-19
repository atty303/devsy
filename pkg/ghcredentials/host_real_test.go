package ghcredentials

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireRealGH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gh"); err != nil {
		if os.Getenv("DEVSY_TEST_REAL_GH") == "1" {
			require.NoError(t, err)
		}
		t.Skip("real gh is not installed; CI requires this native test")
	}
}

func TestNativeRealCLIStoredAccountsIgnoreHostTokenEnvironment(t *testing.T) {
	requireRealGH(t)
	dir := filepath.Join(t.TempDir(), "config with spaces")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, hostsFile), []byte(`github.com:
    user: stored-user
    oauth_token: dummy-stored-token
    users:
        stored-user:
            oauth_token: dummy-stored-token
enterprise.example.invalid:
    user: enterprise-user
    oauth_token: dummy-enterprise-token
`), 0o600))
	t.Setenv("GH_CONFIG_DIR", dir)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(name, "dummy-unrelated-environment-token")
	}
	result := HostAccounts(context.Background())
	require.Equal(t, stateAvailable, result.Status)
	require.Len(t, result.Accounts, 2)
	// Boolean assertions keep even the generated tokens out of failure output.
	require.True(
		t,
		result.Accounts[0] == (Account{Host: "github.com", User: "stored-user", Token: "dummy-stored-token"}),
	)
	require.True(
		t,
		result.Accounts[1] == (Account{
			Host: "enterprise.example.invalid", User: "enterprise-user", Token: "dummy-enterprise-token",
		}),
	)
	require.Equal(t, "dummy-unrelated-environment-token", os.Getenv("GH_TOKEN"))
}
