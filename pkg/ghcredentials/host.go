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

// HostAccounts reads active accounts through the host's unmodified gh. It never
// runs login, refresh, switch, logout, or an API request.
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
	b, err := readFile(filepath.Join(dir, hostsFile))
	if err != nil {
		return HostResult{Status: "config_read_" + errorKind(err)}
	}
	hosts, err := decode(b)
	if err != nil {
		return HostResult{Status: "config_decode_failed"}
	}
	return collectHostAccounts(ctx, binary, hosts)
}

func collectHostAccounts(ctx context.Context, binary string, hosts *yaml.Node) HostResult {
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
		token, err := resolveToken(ctx, binary, host, os.Environ())
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
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
