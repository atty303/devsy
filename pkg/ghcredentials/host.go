package ghcredentials

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Account struct {
	Host  string `json:"host"`
	User  string `json:"user"`
	Token string `json:"token"`
}

func HostAccounts(ctx context.Context) (result HostResult) {
	started := time.Now()
	info := HostContext{Inputs: credentialInputs()}
	defer func() { result.Context = info; result.DurationMS = time.Since(started).Milliseconds() }()
	binary, err := exec.LookPath("gh")
	if err != nil {
		return HostResult{Status: "gh_not_found"}
	}
	info.Binary = binary
	dir, err := ConfigDir()
	if err != nil {
		return HostResult{Status: "config_path_failed"}
	}
	info.ConfigDir = dir
	probe, err := os.MkdirTemp("", "devsy-gh-host-")
	if err != nil {
		return HostResult{Status: "config_snapshot_" + errorKind(err)}
	}
	defer func() { _ = os.RemoveAll(probe) }()
	if err := copyConfigFiles(dir, probe); err != nil {
		return HostResult{Status: "config_snapshot_" + errorKind(err)}
	}
	b, err := readFile(filepath.Join(probe, hostsFile))
	if err != nil {
		return HostResult{Status: "config_read_" + errorKind(err)}
	}
	hosts, err := decode(b)
	if err != nil {
		return HostResult{Status: "config_decode_failed"}
	}
	return collectHostAccounts(ctx, binary, hosts, replaceEnv(os.Environ(), "GH_CONFIG_DIR", probe))
}

func collectHostAccounts(
	ctx context.Context,
	binary string,
	hosts *yaml.Node,
	env []string,
) HostResult {
	result := HostResult{Status: "no_active_accounts"}
	for i := 0; i+1 < len(hosts.Content); i += 2 {
		host := hosts.Content[i].Value
		u := get(hosts.Content[i+1], "user")
		if u == nil || u.Value == "" {
			result.Decisions = append(
				result.Decisions,
				hostDecision{Host: host, Source: "no_active_account"},
			)
			continue
		}
		token, err := resolveToken(ctx, binary, host, env)
		if err != nil {
			result.Decisions = append(
				result.Decisions,
				hostDecision{Host: host, Source: "token_" + errorKind(err)},
			)
			continue
		}
		if token != "" {
			result.Accounts = append(
				result.Accounts,
				Account{Host: host, User: u.Value, Token: token},
			)
		}
	}
	if len(result.Accounts) > 0 {
		result.Status = "available"
	}
	return result
}

func resolveToken(ctx context.Context, binary, host string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	//nolint:gosec // binary is resolved from the user's PATH; host is one argument.
	cmd := exec.CommandContext(ctx, binary, "auth", "token", "--hostname", host)
	// Scope this to the lookup child: nested wrappers must not return forwarded
	// credentials as evidence of local authentication.
	cmd.Env = replaceEnv(env, authProbeEnv, "1")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
