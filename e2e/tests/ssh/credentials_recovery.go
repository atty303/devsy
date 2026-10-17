package ssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/devsy-org/devsy/e2e/framework"
	"github.com/devsy-org/devsy/pkg/docker"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func relayCommand(ctx context.Context, wd, root string) *exec.Cmd {
	f := framework.NewDefaultFramework(filepath.Join(wd, "bin"))
	command := exec.CommandContext(ctx, "bash", "../hack/ci/credential-relay.sh")
	command.Env = append(os.Environ(),
		"RELAY_ROOT="+root,
		"RELAY_HOST_BINARY="+filepath.Join(f.DevsyBinDir, f.DevsyBinName),
		"RELAY_AGENT_BINARY="+filepath.Join(wd, "bin", "devsy-linux-"+runtime.GOARCH),
	)
	docker.PrepareForGroupCancellation(command)
	command.WaitDelay = 30 * time.Second
	command.Stdout = ginkgo.GinkgoWriter
	command.Stderr = ginkgo.GinkgoWriter
	return command
}

func cleanupRelay(ctx context.Context, command *exec.Cmd) error {
	cleanup := exec.CommandContext(ctx, "bash", "../hack/ci/credential-relay.sh", "--cleanup")
	cleanup.Env = command.Env
	docker.PrepareForGroupCancellation(cleanup)
	cleanup.WaitDelay = 30 * time.Second
	cleanup.Stdout = ginkgo.GinkgoWriter
	cleanup.Stderr = ginkgo.GinkgoWriter
	return cleanup.Run()
}

var _ = ginkgo.Describe(
	"credential relay recovery",
	ginkgo.Label("ssh", "ssh-credentials-recovery"),
	func() {
		var wd, root string
		ginkgo.BeforeEach(func() {
			if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
				ginkgo.Skip("requires the Linux amd64 SSH CI environment")
			}
			var err error
			wd, err = os.Getwd()
			framework.ExpectNoError(err)
			root, err = os.MkdirTemp("", "relay-")
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(func() { framework.ExpectNoError(os.RemoveAll(root)) })
		})
		ginkgo.It("retains credential forwarding after the owning SSH connection ends",
			ginkgo.SpecTimeout(framework.TimeoutLong()), func(ctx context.Context) {
				command := relayCommand(ctx, wd, root)
				ginkgo.DeferCleanup(
					func(cleanupCtx context.Context) { framework.ExpectNoError(cleanupRelay(cleanupCtx, command)) },
				)
				framework.ExpectNoError(command.Run())
			})
		ginkgo.It("allows independent cleanup after the test harness is canceled",
			ginkgo.SpecTimeout(framework.TimeoutLong()), func(ctx context.Context) {
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				command := relayCommand(runCtx, wd, root)
				command.Env = append(command.Env, "RELAY_WAIT_FOR_CANCEL=1")
				ginkgo.DeferCleanup(
					func(cleanupCtx context.Context) { framework.ExpectNoError(cleanupRelay(cleanupCtx, command)) },
				)
				done := make(chan error, 1)
				go func() { done <- command.Run() }()
				gomega.Eventually(func() bool { _, err := os.Stat(filepath.Join(root, "waiters-ready")); return err == nil }).
					WithContext(ctx).
					WithTimeout(framework.TimeoutLong()).
					Should(gomega.BeTrue())
				directory, err := os.OpenRoot(root)
				framework.ExpectNoError(err)
				defer func() { _ = directory.Close() }()
				pids, err := directory.ReadFile("ssh-pids")
				framework.ExpectNoError(err)
				cancel()
				gomega.Eventually(done).
					WithContext(ctx).
					WithTimeout(35 * time.Second).
					Should(gomega.Receive(gomega.HaveOccurred()))
				cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				framework.ExpectNoError(cleanupRelay(cleanupCtx, command))
				check := exec.CommandContext(
					cleanupCtx,
					"bash",
					"../hack/ci/credential-relay.sh",
					"--assert-clean",
				)
				check.Env = append(
					command.Env,
					"RELAY_SSH_PIDS="+strings.ReplaceAll(string(pids), "\n", " "),
				)
				check.Stdout = ginkgo.GinkgoWriter
				check.Stderr = ginkgo.GinkgoWriter
				framework.ExpectNoError(check.Run())
			})
	},
)
