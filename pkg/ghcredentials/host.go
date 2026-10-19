package ghcredentials

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/devsy-org/devsy/pkg/log"
	"gopkg.in/yaml.v3"
)

const hostNoActiveAccounts = "no_active_accounts"

type Account struct {
	Host  string `json:"host"`
	User  string `json:"user"`
	Token string `json:"token"`
}

func HostAccounts(ctx context.Context) HostResult {
	binary, err := exec.LookPath("gh")
	if err != nil {
		return HostResult{Status: "gh_not_found"}
	}
	dir, err := ConfigDir()
	if err != nil {
		return HostResult{Status: "config_path_failed"}
	}
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
	result := HostResult{Status: hostNoActiveAccounts}
	for i := 0; i+1 < len(hosts.Content); i += 2 {
		host := hosts.Content[i].Value
		u := get(hosts.Content[i+1], "user")
		if u == nil || u.Value == "" {
			log.Debugw("gh relay: host account unavailable", "host", host)
			continue
		}
		account, err := (Account{Host: host, User: u.Value}).resolveHostToken(ctx, binary, env)
		if err != nil {
			result.Status = "token_lookup_failed"
		} else if account.valid() {
			result.Accounts = append(result.Accounts, account)
		}
	}
	if len(result.Accounts) > 0 && result.Status == hostNoActiveAccounts {
		result.Status = "available"
	}
	return result
}

func (a Account) resolveHostToken(
	ctx context.Context,
	binary string,
	env []string,
) (Account, error) {
	token, err := resolveToken(ctx, binary, a.Host, env)
	if err != nil {
		if noTokenResult(err) {
			log.Debugw("gh relay: host account unavailable", "host", a.Host)
			return Account{}, nil
		}
		log.Warnw(
			"gh relay: host token lookup failed",
			"host",
			a.Host,
			"error_type",
			errorKind(err),
		)
		return Account{}, err
	}
	a.Token = token
	return a, nil
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
