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
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

type session struct {
	Original  string    `json:"original"`
	Accounts  []Account `json:"accounts"`
	dir       string
	lock      *ownerLock
	decisions []hostDecision
	writeback *writebackSummary
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
	recoverSessions(ctx, root, false)
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

type recoveryDecision struct {
	Session   string            `json:"session"`
	ConfigDir string            `json:"config_dir,omitempty"`
	Outcome   string            `json:"outcome"`
	Operation string            `json:"operation,omitempty"`
	ErrorType string            `json:"error_type,omitempty"`
	Writeback *writebackSummary `json:"writeback,omitempty"`
}

func recoverSessions(ctx context.Context, root string, recording bool) []recoveryDecision {
	var results []recoveryDecision
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "session-") {
			results = append(results, recoverSession(ctx, filepath.Join(root, e.Name()), recording))
		}
	}
	return results
}

func recoverSession(ctx context.Context, dir string, recording bool) recoveryDecision {
	result := recoveryDecision{Session: filepath.Base(dir), Outcome: "active"}
	owner, ok, err := tryOwner(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return recoveryFailure(result, "recovery.owner_lock", err)
	}
	if !ok {
		return result
	}
	defer func() { _ = owner.Close() }()
	return recoverUnlocked(ctx, dir, recording)
}

func recoverUnlocked(ctx context.Context, dir string, recording bool) recoveryDecision {
	result := recoveryDecision{Session: filepath.Base(dir), Outcome: "recovered"}
	old, err := loadSession(dir)
	if err != nil {
		return recoveryFailure(result, "recovery.metadata", err)
	}
	if old == nil {
		_ = os.RemoveAll(dir)
		return result
	}
	result.ConfigDir = old.Original
	if recording {
		old.writeback = &writebackSummary{}
	}
	err = old.sync(ctx)
	result.Writeback = old.writeback
	if err != nil {
		return recoveryFailure(result, "recovery.writeback", err)
	}
	_ = os.RemoveAll(dir)
	return result
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

func recoveryFailure(result recoveryDecision, name string, err error) recoveryDecision {
	result.Outcome = "pending"
	result.Operation, result.ErrorType = describeError(name, err)
	return result
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
			if get(users.Content[i], "oauth_token") != nil {
				return "local_config"
			}
		}
	}
	if user := get(n, "user"); user != nil && user.Value != "" {
		return "local_registration"
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
	var injected []Account
	for _, a := range s.Accounts {
		source := a.inject(ctx, binary, env, hosts)
		s.decisions = append(s.decisions, hostDecision{Host: a.Host, Source: source})
		if source == "forwarded" {
			injected = append(injected, a)
		}
	}
	s.Accounts = injected
	if err := s.save(); err != nil {
		return err
	}
	if len(injected) == 0 {
		return nil
	}
	b, err = yaml.Marshal(hosts)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (s *session) strip(data []byte) ([]byte, error) {
	hosts, err := decode(data)
	if err != nil {
		return nil, err
	}
	for _, a := range s.Accounts {
		stripAccount(hosts, a)
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

func (a Account) inject(ctx context.Context, binary string, env []string, hosts *yaml.Node) string {
	if a.Host == "" || a.User == "" || a.Token == "" {
		return "invalid_account"
	}
	if source := localAuthSource(get(hosts, a.Host)); source != "" {
		return source
	}
	if token := tokenForHost(ctx, binary, a.Host, env); token != "" {
		return localTokenSource(token, env)
	}
	h := ensure(hosts, a.Host)
	put(h, "user", scalar(a.User))
	put(h, "oauth_token", scalar(a.Token))
	put(ensure(ensure(h, "users"), a.User), "oauth_token", scalar(a.Token))
	return "forwarded"
}

func stripAccount(hosts *yaml.Node, a Account) {
	h := get(hosts, a.Host)
	if h == nil {
		return
	}
	u := get(h, "users")
	account := get(u, a.User)
	activeToken := get(h, "oauth_token")
	userToken := get(account, "oauth_token")
	// Successful keyring login removes both file tokens. Keep its account
	// registration; the keyring itself remains entirely under gh's control.
	keyringLogin := activeToken == nil && userToken == nil
	removeForwardedToken(h, a.Token)
	removeForwardedToken(account, a.Token)
	if !keyringLogin {
		stripRegistration(h, u, account, a.User)
	}
	if len(h.Content) == 0 {
		put(hosts, a.Host, nil)
	}
}

func removeForwardedToken(n *yaml.Node, token string) {
	if t := get(n, "oauth_token"); t != nil && t.Value == token {
		put(n, "oauth_token", nil)
	}
}

func stripRegistration(host, users, account *yaml.Node, user string) {
	if t := get(host, "user"); t != nil && t.Value == user && get(account, "oauth_token") == nil {
		put(host, "user", nil)
	}
	if account != nil && len(account.Content) == 0 {
		put(users, user, nil)
	}
	if users != nil && len(users.Content) == 0 {
		put(host, "users", nil)
	}
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
		after, err = s.strip(after)
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
		s.writeback.unchanged()
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
	s.writeback.written(err)
	return boundary("config.write", err)
}

func (s *session) mergeFile(name string, before, after, latest []byte) ([]byte, error) {
	if err := s.writeback.inspect(name, before, after, latest); err != nil {
		return nil, err
	}
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
