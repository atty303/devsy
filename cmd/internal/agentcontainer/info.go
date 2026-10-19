package agentcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/devsy-org/devsy/cmd/flags"
	"github.com/devsy-org/devsy/pkg/clierr"
	"github.com/devsy-org/devsy/pkg/config"
	devconfig "github.com/devsy-org/devsy/pkg/devcontainer/config"
	"github.com/devsy-org/devsy/pkg/output"
	"github.com/devsy-org/devsy/pkg/table"
	"github.com/spf13/cobra"
)

type containerInfo struct {
	SSHCommand   string  `json:"sshCommand"`
	SSHHost      string  `json:"sshHost"`
	RemoteUser   string  `json:"remoteUser"`
	CheckoutRoot *string `json:"checkoutRoot"`
	Branch       *string `json:"branch"`
	Commit       *string `json:"commit"`
}

func NewInfoCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show this container's SSH connection and current Git checkout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := output.ResolveMode(globalFlags.ResultFormat)
			if err != nil {
				return err
			}
			info, err := readContainerInfo(cmd.Context())
			if err != nil {
				classified := clierr.Classify(err)
				if classified.Code == clierr.CodeUnknown {
					classified.Code = "container_info_failed"
				}
				return classified
			}
			return writeContainerInfo(cmd.OutOrStdout(), mode, info)
		},
	}
}

func readContainerInfo(ctx context.Context) (*containerInfo, error) {
	workspaceID := os.Getenv(config.EnvWorkspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("%s is missing; run inside a Devsy container", config.EnvWorkspaceID)
	}
	result, err := readInfoSetupResult(ctx)
	if err != nil {
		return nil, err
	}
	info := &containerInfo{
		SSHHost:    workspaceID + config.SSHHostSuffix,
		RemoteUser: devconfig.GetRemoteUser(result),
	}
	info.SSHCommand = "ssh " + shellescape.Quote(info.RemoteUser+"@"+info.SSHHost)
	if err := info.readCheckout(ctx); err != nil {
		return nil, err
	}
	return info, nil
}

func readInfoSetupResult(ctx context.Context) (*devconfig.Result, error) {
	command := exec.CommandContext(ctx, "sh")
	command.Args = append(command.Args, "-c", config.ReadDevContainerResultCommand())
	raw, err := command.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf(
				"read selected container setup result: %w: %s",
				err,
				strings.TrimSpace(string(exit.Stderr)),
			)
		}
		return nil, fmt.Errorf("read selected container setup result: %w", err)
	}
	result := &devconfig.Result{}
	if err := json.Unmarshal(raw, result); err != nil {
		return nil, fmt.Errorf("parse container setup result: %w", err)
	}
	if result.MergedConfig == nil || result.ContainerDetails == nil || result.Error != "" {
		return nil, fmt.Errorf("container setup result is incomplete or failed")
	}
	return result, nil
}

func writeContainerInfo(w io.Writer, mode string, info *containerInfo) error {
	if mode == output.ModeJSON {
		return json.NewEncoder(w).Encode(info)
	}
	rows := [][]string{
		{"SSH command", info.SSHCommand},
		{"SSH host", info.SSHHost},
		{"Remote user", info.RemoteUser},
		{"Checkout root", infoValue(info.CheckoutRoot)},
		{"Branch", infoValue(info.Branch)},
		{"Commit", infoValue(info.Commit)},
	}
	_, err := fmt.Fprintln(w, table.Render([]string{"Field", "Value"}, rows))
	return err
}

func infoValue(value *string) string {
	if value == nil {
		return "(none)"
	}
	return *value
}

func (info *containerInfo) readCheckout(ctx context.Context) error {
	if err := validateCheckoutMetadata(ctx); err != nil {
		return err
	}
	inside, err := infoGit(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if infoGitOutsideCheckout(err) {
			return nil
		}
		return fmt.Errorf("resolve Git checkout root: %w", err)
	}
	if inside == "false" {
		return nil
	}
	root, err := infoGit(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("resolve Git checkout root: %w", err)
	}
	info.CheckoutRoot = &root
	return info.readGitHead(ctx)
}

func infoGitOutsideCheckout(err error) bool {
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 128 {
		return false
	}
	for line := range strings.SplitSeq(string(exit.Stderr), "\n") {
		if strings.HasPrefix(line, "fatal: not a git repository (or any ") {
			return true
		}
	}
	return false
}

func (info *containerInfo) readGitHead(ctx context.Context) error {
	ref, err := infoGit(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil && !infoGitExit(err, 1) {
		return fmt.Errorf("resolve Git branch: %w", err)
	}
	if err == nil {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		info.Branch = &branch
	}
	commit, err := infoGit(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		if info.Branch != nil {
			_, refErr := infoGit(ctx, "show-ref", "--verify", "--quiet", ref)
			if infoGitExit(refErr, 1) {
				return nil
			}
		}
		return fmt.Errorf("resolve Git commit: %w", err)
	}
	info.Commit = &commit
	return nil
}

func infoGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = infoGitEnv()
	raw, err := cmd.Output()
	value := strings.TrimSuffix(string(raw), "\n")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return value, fmt.Errorf(
				"git %s: %w: %s",
				strings.Join(args, " "),
				err,
				strings.TrimSpace(string(exit.Stderr)),
			)
		}
		return value, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return value, nil
}

func infoGitExit(err error, code int) bool {
	exit, ok := errors.AsType[*exec.ExitError](err)
	return ok && exit.ExitCode() == code
}

func infoGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_PREFIX", "LC_ALL":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "LC_ALL=C")
}

func validateCheckoutMetadata(ctx context.Context) error {
	metadata, err := findCheckoutMetadata()
	if err != nil || metadata == "" {
		return err
	}
	if _, err := infoGit(ctx, "rev-parse", "--resolve-git-dir", metadata); err != nil {
		return fmt.Errorf("resolve Git checkout metadata: %w", err)
	}
	return nil
}

func findCheckoutMetadata() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve Git checkout directory: %w", err)
	}
	for {
		metadata := filepath.Join(dir, ".git")
		_, err := os.Lstat(metadata)
		if err == nil {
			return metadata, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect Git checkout metadata: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}
