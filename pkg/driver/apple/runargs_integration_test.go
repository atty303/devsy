package apple

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/devsy-org/devsy/pkg/apple"
	"github.com/devsy-org/devsy/pkg/devcontainer/config"
	"github.com/devsy-org/devsy/pkg/driver"
	"github.com/stretchr/testify/require"
)

func TestTmpfsMountsIntegration(t *testing.T) {
	if os.Getenv("DEVSY_APPLE_E2E") != "1" {
		t.Skip("set DEVSY_APPLE_E2E=1 to run against a real container CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	helper := &apple.AppleHelper{Command: "container"}
	name := fmt.Sprintf("devsy-tmpfs-%d", time.Now().UnixNano())
	params := tmpfsIntegrationParams(name)
	args := (&appleDriver{}).buildRunArgs(params)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		found, findErr := helper.FindContainerByID(cleanupCtx, []string{name})
		require.NoError(t, findErr)
		if found != nil {
			_ = helper.Stop(cleanupCtx, name)
			require.NoError(t, helper.Remove(cleanupCtx, name))
		}
	})
	var out bytes.Buffer
	err := helper.Run(ctx, args, apple.Streams{Stdout: &out, Stderr: &out})
	require.NoError(t, err, out.String())
	out.Reset()
	err = helper.Run(
		ctx,
		[]string{"exec", name, "sh", "-c", tmpfsIntegrationOracle},
		apple.Streams{Stdout: &out, Stderr: &out},
	)
	require.NoError(t, err, out.String())
	t.Log(out.String())
}

func tmpfsIntegrationParams(name string) *driver.RunImageDevContainerParams {
	return &driver.RunImageDevContainerParams{
		WorkspaceID: name,
		ParsedConfig: &config.DevContainerConfig{
			NonComposeBase: config.NonComposeBase{
				RunArgs: []string{"--name", name, "--cpus", "1", "--memory", "256M"},
			},
		},
		Options: &driver.RunOptions{
			Image: "ubuntu:24.04", Entrypoint: "/bin/sh", Cmd: []string{"-c", "sleep 120"},
			Mounts: []*config.Mount{
				{
					Type:   driver.MountTypeTmpfs,
					Target: config.SecretsMountDir,
					Other:  []string{"tmpfs-mode=0755"},
				},
				{
					Type:   driver.MountTypeTmpfs,
					Target: config.SecretsEnvDir,
					Other:  []string{"tmpfs-mode=0700"},
				},
				{
					Type:   driver.MountTypeTmpfs,
					Target: "/probe-native",
					Other:  []string{"mode=0700", "size=1M"},
				},
				{
					Type:   driver.MountTypeTmpfs,
					Target: "/probe-size",
					Other:  []string{"tmpfs-size=1M", "tmpfs-size=2M"},
				},
				{Type: driver.MountTypeTmpfs, Target: "/probe-ro", Other: []string{"readonly"}},
			},
		},
	}
}

const tmpfsIntegrationOracle = `set -eu
stat -c '%n %a' /run/secrets /run/devsy/secrets-env /probe-native
stat -f -c '%n %T %S %b' /probe-native /probe-size
test "$(stat -c %a /run/secrets)" = 755
test "$(stat -c %a /run/devsy/secrets-env)" = 700
test "$(stat -c %a /probe-native)" = 700
for p in /run/secrets /run/devsy/secrets-env /probe-native /probe-size /probe-ro; do
 test "$(stat -f -c %T "$p")" = tmpfs
done
test "$(stat -f -c '%S:%b' /probe-native)" = 4096:256
test "$(stat -f -c '%S:%b' /probe-size)" = 4096:512
touch /probe-native/writable
if touch /probe-ro/forbidden 2>/dev/null; then exit 1; fi
echo 'native CLI tmpfs mount behavior verified'
`
