package containerinfo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"al.essio.dev/pkg/shellescape"
	"github.com/devsy-org/devsy/e2e/framework"
	"github.com/devsy-org/devsy/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	containerCommand = "container"
	infoCommand      = "info"
)

var _ = ginkgo.Describe("container connection information", ginkgo.Label("container-info"), func() {
	ginkgo.It(
		"should show the container's SSH connection details",
		ginkgo.SpecTimeout(framework.TimeoutShort()),
		ginkgo.Label("integration"),
		func(ctx context.Context) {
			t := ginkgo.GinkgoT()
			dir := t.TempDir()
			fixture := filepath.Join(dir, "setup-result.json")
			framework.ExpectNoError(
				os.WriteFile(
					fixture,
					[]byte(
						`{
 "MergedConfig":{"remoteUser":"configured-user","remoteEnv":{"TOKEN":"do-not-output"}},
 "ContainerDetails":{"Config":{"User":"image-user"}}
}`,
					),
					0o600,
				),
			)
			script, err := os.CreateTemp(dir, "setup-reader-*")
			framework.ExpectNoError(err)
			_, err = script.WriteString(
				"#!/bin/sh\nexec /bin/cat " + shellescape.Quote(fixture) + "\n",
			)
			framework.ExpectNoError(err)
			framework.ExpectNoError(script.Chmod(0o700))
			framework.ExpectNoError(script.Close())
			framework.ExpectNoError(os.Symlink(script.Name(), filepath.Join(dir, "sh")))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv(config.EnvWorkspaceID, "test-workspace")
			t.Setenv("USER", "wrong-os-user")
			t.Setenv("LOGNAME", "wrong-os-user")
			t.Setenv("DEVSY_UI", "")
			cwd, err := os.Getwd()
			framework.ExpectNoError(err)
			f := framework.NewDefaultFramework(filepath.Join(cwd, "bin"))
			for _, entry := range [][]string{
				{containerCommand, infoCommand},
				{"internal", "agent", containerCommand, infoCommand},
			} {
				args := append(
					entry,
					"--home",
					t.TempDir(),
					"--context",
					"not-configured",
					"--provider",
					"not-configured",
					"--result-format",
					"json",
				)
				stdout, stderr, err := f.ExecCommandCapture(ctx, args)
				framework.ExpectNoError(err)
				gomega.Expect(stderr).To(gomega.BeEmpty())
				var got map[string]any
				framework.ExpectNoError(json.Unmarshal([]byte(stdout), &got))
				gomega.Expect(got).To(gomega.Equal(map[string]any{
					"sshCommand": "ssh configured-user@test-workspace.devsy",
					"sshHost":    "test-workspace.devsy", "remoteUser": "configured-user",
				}))
				args[len(args)-1] = "plain"
				stdout, stderr, err = f.ExecCommandCapture(ctx, args)
				framework.ExpectNoError(err)
				gomega.Expect(stderr).To(gomega.BeEmpty())
				for _, value := range []string{
					"Field", "Value", "SSH command", "SSH host", "Remote user",
					"ssh configured-user@test-workspace.devsy",
				} {
					gomega.Expect(stdout).To(gomega.ContainSubstring(value))
				}
				for _, omitted := range []string{
					"do-not-output", "wrong-os-user",
				} {
					gomega.Expect(stdout).NotTo(gomega.ContainSubstring(omitted))
				}
			}
			t.Setenv(config.EnvWorkspaceID, "")
			stdout, stderr, err := f.ExecCommandCapture(
				ctx,
				[]string{
					containerCommand,
					"info",
					"--home",
					t.TempDir(),
					"--result-format",
					"json",
				},
			)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(stdout).To(gomega.BeEmpty())
			gomega.Expect(stderr).To(gomega.ContainSubstring(config.EnvWorkspaceID))
		},
	)
})
