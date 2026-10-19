package ghcredentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/devsy-org/devsy/pkg/log"
	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

type session struct {
	Original string    `json:"original"`
	Accounts []Account `json:"accounts"`
	dir      string
	lock     *ownerLock
}

func sessionRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "devsy", "gh-relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func lock(ctx context.Context, path string) (*flock.Flock, error) {
	l := flock.New(path)
	ok, err := l.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("GitHub CLI configuration lock unavailable")
	}
	return l, nil
}

func startSession(ctx context.Context, original string, accounts []Account) (*session, error) {
	root, err := sessionRoot()
	if err != nil {
		return nil, err
	}
	l, err := lock(ctx, filepath.Join(root, "sessions.lock"))
	if err != nil {
		return nil, boundary("session.catalog_lock", err)
	}
	defer func() { _ = l.Close() }()
	recoverSessions(ctx, root)
	return newSession(ctx, root, original, accounts)
}

func newSession(ctx context.Context, root, original string, accounts []Account) (*session, error) {
	dir, err := os.MkdirTemp(root, "session-")
	if err != nil {
		return nil, err
	}
	owner, err := lockOwner(filepath.Join(dir, "owner.lock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, boundary("session.owner_lock", err)
	}
	s := &session{Original: original, Accounts: accounts, dir: dir, lock: owner}
	ready := false
	defer func() {
		if !ready {
			_ = owner.Close()
			_ = os.RemoveAll(dir)
		}
	}()
	if err := copyTree(ctx, original, filepath.Join(dir, "before"), 0); err != nil {
		return nil, boundary("snapshot.read", err)
	}
	if err := copyTree(ctx, filepath.Join(dir, "before"), s.work(), 0); err != nil {
		return nil, boundary("snapshot.copy", err)
	}
	initial := *s
	initial.Accounts = nil // No forwarded values exist until inject completes.
	if err := initial.save(); err != nil {
		return nil, boundary("session.metadata", err)
	}
	ready = true
	return s, nil
}

func (s *session) work() string { return filepath.Join(s.dir, "work") }

func copyTree(ctx context.Context, src, dst string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 32 {
		return errors.New("GitHub CLI configuration symlink cycle")
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyEntry(
			ctx,
			filepath.Join(src, e.Name()),
			filepath.Join(dst, e.Name()),
			depth,
		); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(ctx context.Context, src, dst string, depth int) error {
	info, err := os.Stat(src) // Follow symlinks into a private copy, never a link to the original.
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyTree(ctx, src, dst, depth+1)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > 16<<20 {
		return errors.New("GitHub CLI configuration file too large")
	}
	data, err := readFile(src)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if info.Mode()&0o111 != 0 {
		mode = 0o700
	}
	return os.WriteFile(dst, data, mode)
}

func recoverSessions(ctx context.Context, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		logFailure("gh relay: recovery pending", "recovery.list", err, true)
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "session-") {
			dir := filepath.Join(root, e.Name())
			if err := recoverSession(ctx, dir); err != nil {
				logFailure("gh relay: recovery pending", "recovery.writeback", err, true)
			}
		}
	}
}

func recoverSession(ctx context.Context, dir string) error {
	owner, ok, err := tryOwner(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return boundary("recovery.owner_lock", err)
	}
	if !ok {
		return nil
	}
	defer func() { _ = owner.Close() }()
	old, err := loadSession(dir)
	if err != nil {
		return boundary("recovery.metadata", err)
	}
	if old != nil {
		if err := old.sync(ctx); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return boundary("recovery.cleanup", err)
	}
	log.Debug("gh relay: configuration recovered")
	return nil
}

func loadSession(dir string) (*session, error) {
	b, err := readFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	var old session
	if json.Unmarshal(b, &old) != nil || !filepath.IsAbs(old.Original) {
		return nil, errInvalidConfig
	}
	old.dir = dir
	return &old, nil
}

func (s *session) save() error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "session.json"), b, 0o600)
}

func localAuthSource(n *yaml.Node) string {
	if value := get(n, "oauth_token"); value != nil && value.Value != "" {
		return "local_config"
	}
	users := get(n, "users")
	if users != nil {
		for i := 1; i < len(users.Content); i += 2 {
			if token := get(users.Content[i], "oauth_token"); token != nil && token.Value != "" {
				return "local_config"
			}
		}
	}
	return ""
}

func mapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

func ensure(n *yaml.Node, key string) *yaml.Node {
	v := get(n, key)
	if v == nil || v.Kind != yaml.MappingNode {
		v = mapping()
		put(n, key, v)
	}
	return v
}

func (s *session) inject(ctx context.Context, binary string, env []string) error {
	path := filepath.Join(s.work(), hostsFile)
	b, err := readFile(path)
	if err != nil {
		return boundary("config.read", err)
	}
	hosts, err := decode(b)
	if err != nil {
		return boundary("config.decode", err)
	}
	if err := s.overlay(ctx, binary, env, hosts); err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	if len(s.Accounts) == 0 {
		return nil
	}
	b, err = yaml.Marshal(hosts)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Even gh auth token can migrate configuration. Never point the lookup at
// original settings or the immutable writeback baseline. Each host gets a fresh
// copy because a migration may change registrations for other hosts as well.
func (s *session) overlay(
	ctx context.Context,
	binary string,
	env []string,
	hosts *yaml.Node,
) error {
	var injected []Account
	for _, a := range s.Accounts {
		probe, err := s.probeConfig()
		if err != nil {
			return boundary("config.probe_snapshot", err)
		}
		source := a.inject(ctx, binary, replaceEnv(env, "GH_CONFIG_DIR", probe), hosts)
		_ = os.RemoveAll(probe)
		log.Debugw("gh relay: authentication source", "host", a.Host, "source", source)
		if source == "forwarded" {
			injected = append(injected, a)
		}
	}
	s.Accounts = injected
	return nil
}

func (s *session) probeConfig() (string, error) {
	dir, err := os.MkdirTemp(s.dir, "probe-")
	if err != nil {
		return "", err
	}
	if err := copyConfigFiles(filepath.Join(s.dir, "before"), dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func copyConfigFiles(src, dst string) error {
	for _, name := range []string{hostsFile, configFile} {
		b, err := readFile(filepath.Join(src, name))
		if err != nil {
			return err
		}
		if b != nil {
			if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session) strip(before, data []byte) ([]byte, error) {
	original, err := decode(before)
	if err != nil {
		return nil, err
	}
	hosts, err := decode(data)
	if err != nil {
		return nil, err
	}
	for _, a := range s.Accounts {
		stripAccount(hosts, get(original, a.Host), a)
	}
	return yaml.Marshal(hosts)
}

func files(dir string) (map[string]bool, error) {
	result := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !e.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			result[rel] = true
		}
		return nil
	})
	return result, err
}

func (s *session) sync(ctx context.Context) error {
	root := filepath.Dir(s.dir)
	key := sha256.Sum256([]byte(s.Original))
	l, err := lock(ctx, filepath.Join(root, fmt.Sprintf("config-%x.lock", key[:16])))
	if err != nil {
		return boundary("config.lock", err)
	}
	defer func() { _ = l.Close() }()
	beforeDir := filepath.Join(s.dir, "before")
	names, err := files(beforeDir)
	if err != nil {
		return err
	}
	afterNames, err := files(s.work())
	if err != nil {
		return err
	}
	for name := range afterNames {
		names[name] = true
	}
	for name := range names {
		if err := s.syncFile(name); err != nil {
			return err
		}
	}
	return nil
}

func writeOriginal(path string, data []byte, mode fs.FileMode) error {
	var err error
	path, err = resolveOriginal(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".devsy-gh-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *session) finish(ctx context.Context) error {
	err := s.sync(ctx)
	if err == nil {
		_ = os.RemoveAll(s.dir)
	}
	_ = s.lock.Close()
	return err
}

func (a Account) inject(
	ctx context.Context,
	binary string,
	env []string,
	hosts *yaml.Node,
) string {
	h := get(hosts, a.Host)
	if !a.valid() {
		return "invalid_account"
	}
	if source := localAuthSource(h); source != "" {
		return source
	}
	token, err := resolveToken(ctx, binary, a.Host, env)
	if err != nil && !noTokenResult(err) {
		log.Warnw(
			"gh relay: forwarding unavailable",
			"operation",
			"local.token_lookup",
			"host",
			a.Host,
			"error_type",
			errorKind(err),
		)
		return "local_lookup_failed"
	}
	if token != "" {
		return localTokenSource(token, env)
	}
	a.overlay(hosts)
	return "forwarded"
}

// gh auth token reports an absent token as exit 1. It also collapses keyring
// backend errors into that result; the CLI cannot distinguish those cases.
// Execution failures and timeouts are not evidence of absent authentication.
func noTokenResult(err error) bool {
	exit, ok := errors.AsType[*exec.ExitError](err)
	return ok && exit.ExitCode() == 1
}

func (a Account) valid() bool {
	return a.Host != "" && a.User != "" && a.Token != ""
}

func (a Account) overlay(hosts *yaml.Node) {
	h := ensure(hosts, a.Host)
	put(h, "user", scalar(a.User))
	put(h, "oauth_token", scalar(a.Token))
	// Tokenless registrations are settings, not usable accounts. Hide them
	// only in the overlay so gh auth status does not report them as failures.
	users := mapping()
	put(users, a.User, get(get(h, "users"), a.User))
	put(ensure(users, a.User), "oauth_token", scalar(a.Token))
	put(h, "users", users)
}

func stripAccount(hosts, original *yaml.Node, a Account) {
	h := get(hosts, a.Host)
	if h == nil {
		put(hosts, a.Host, original)
		return
	}
	u := get(h, "users")
	account := get(u, a.User)
	activeToken := get(h, "oauth_token")
	userToken := get(account, "oauth_token")
	// Successful keyring login removes both file tokens. Keep its account
	// registration; the keyring itself remains entirely under gh's control.
	keyringLogin := activeToken == nil && userToken == nil
	removeForwardedToken(h, original, a.Token)
	removeForwardedToken(account, get(get(original, "users"), a.User), a.Token)
	if !keyringLogin {
		stripRegistration(h, get(original, "user"), a.User)
	}
	// Restore hidden dotfiles registrations while retaining new local logins.
	// No real original credential can be hidden: inject excludes those hosts.
	users := get(h, "users")
	if users == nil {
		users = mapping()
	}
	put(h, "users", merge(mapping(), users, get(original, "users")))
	if len(h.Content) == 0 {
		put(hosts, a.Host, nil)
	}
}

func removeForwardedToken(n, original *yaml.Node, token string) {
	if t := get(n, "oauth_token"); t != nil && t.Value == token {
		put(n, "oauth_token", get(original, "oauth_token"))
	}
}

func stripRegistration(host, originalUser *yaml.Node, user string) {
	users := get(host, "users")
	account := get(users, user)
	if forwardedRegistration(host, account, user) {
		put(host, "user", originalUser)
	}
	if account != nil && len(account.Content) == 0 {
		put(users, user, nil)
	}
	if users != nil && len(users.Content) == 0 {
		put(host, "users", nil)
	}
}

func hasToken(n *yaml.Node) bool {
	t := get(n, "oauth_token")
	return t != nil && t.Value != ""
}

func forwardedRegistration(host, account *yaml.Node, user string) bool {
	t := get(host, "user")
	return t != nil && t.Value == user && !hasToken(account) && !hasToken(host)
}

func (s *session) changedFile(name string) ([]byte, []byte, error) {
	before, err := readFile(filepath.Join(s.dir, "before", name))
	if err != nil {
		return nil, nil, boundary("config.snapshot_read", err)
	}
	after, err := readFile(filepath.Join(s.work(), name))
	if err != nil {
		return nil, nil, boundary("config.changed_read", err)
	}
	if name == hostsFile && len(after) != 0 {
		after, err = s.strip(before, after)
		if err != nil {
			return nil, nil, boundary("config.strip", err)
		}
	}
	return before, after, nil
}

func (s *session) syncFile(name string) error {
	before, after, err := s.changedFile(name)
	if err != nil {
		return err
	}

	if bytes.Equal(before, after) {
		return nil
	}
	path := filepath.Join(s.Original, name)
	latest, err := readFile(path)
	if err != nil {
		return boundary("config.latest_read", err)
	}
	after, err = s.mergeFile(name, before, after, latest)
	if err != nil {
		return boundary("config.merge", err)
	}
	if bytes.Equal(latest, after) {
		return nil
	}
	err = s.saveOriginal(name, after)
	return boundary("config.write", err)
}

func (s *session) mergeFile(name string, before, after, latest []byte) ([]byte, error) {
	if name == hostsFile || name == configFile {
		return mergeYAML(before, after, latest)
	}
	return after, nil
}

func (s *session) saveOriginal(name string, after []byte) error {
	path := filepath.Join(s.Original, name)
	if after == nil {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	mode := fs.FileMode(0o600)
	info, err := os.Stat(filepath.Join(s.work(), name))
	if err == nil && info.Mode()&0o111 != 0 {
		mode = 0o700
	}
	return writeOriginal(path, after, mode)
}

func localTokenSource(token string, env []string) string {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if authEnvironment[name] && value == token {
			return "environment"
		}
	}
	return "keyring"
}

var authEnvironment = map[string]bool{
	"GH_TOKEN":                true,
	"GITHUB_TOKEN":            true,
	"GH_ENTERPRISE_TOKEN":     true,
	"GITHUB_ENTERPRISE_TOKEN": true,
}

func resolveOriginal(path string) (string, error) {
	// Resolve the current link target at writeback time, including symlinked
	// parent directories. Never replace a dotfiles manager's symlink.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if _, linkErr := os.Lstat(path); linkErr == nil {
		// A dangling link is still the user's link. Keep the changes pending
		// rather than replacing it with a regular file.
		return "", errors.New("GitHub CLI configuration target unavailable")
	}
	return path, nil
}
