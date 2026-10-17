// Package main assembles and builds the source selected by the fork release workflow.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

const (
	fork         = "atty303/devsy"
	osLinux      = "linux"
	commandBuild = "build"
)

var (
	errConfigInvalid  = errors.New("configuration invalid")
	errRetired        = errors.New("no selected branches: fork retired; no release will be created")
	errSourceMismatch = errors.New("source SHA differs from manifest")
	errProcess        = errors.New("external process failed")
	repoPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	versionPattern    = regexp.MustCompile(`^v0\.0\.0-atty\.([1-9][0-9]*)$`)
)

type resolution struct {
	Before   string `json:"before_tree"`
	Incoming string `json:"incoming"`
	Patch    string `json:"patch"`
}

type branch struct {
	Repository string      `json:"repository"`
	Branch     string      `json:"branch"`
	SHA        string      `json:"sha,omitempty"`
	Checks     []string    `json:"checks"`
	Resolution *resolution `json:"resolution,omitempty"`
}

type config struct {
	Upstream       string   `json:"upstream"`
	UpstreamBranch string   `json:"upstream_branch"`
	Branches       []branch `json:"branches"`
	Patches        []string `json:"patches"`
}

type manifest struct {
	Version          string            `json:"version"`
	ManagementSHA    string            `json:"management_sha"`
	UpstreamSHA      string            `json:"upstream_sha"`
	SourceSHA        string            `json:"source_sha"`
	Config           config            `json:"config"`
	PatchHashes      map[string]string `json:"patch_sha256"`
	Checks           []string          `json:"checks"`
	RunURL           string            `json:"run_url,omitempty"`
	SigningPublicKey string            `json:"signing_public_key"`
}

type operation struct {
	Time       string            `json:"time"`
	Name       string            `json:"operation"`
	Status     string            `json:"status"`
	Arguments  []string          `json:"arguments"`
	ErrorType  string            `json:"error_type,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type runner struct{ record io.Writer }

func (r runner) run(
	dir string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	output, err := c.CombinedOutput()
	status, kind := "ok", ""
	if err != nil {
		status, kind = "error", "process_exit"
	}
	if r.record != nil {
		// Only callers' public Git refs, paths, build flags and commands are recorded; never environment values.
		r.observe(operation{Name: name, Status: status, Arguments: args, ErrorType: kind})
	}
	if err != nil {
		return output, fmt.Errorf(
			"%w: %s %s: %w\n%s",
			errProcess,
			name,
			strings.Join(args, " "),
			err,
			output,
		)
	}
	return output, nil
}

func (r runner) git(dir string, args ...string) (string, error) {
	b, err := r.run(dir, nil, "git", args...)
	return strings.TrimSpace(string(b)), err
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func safePath(root, path string) (string, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if invalidPatchPath(path) {
		return "", fmt.Errorf("invalid relative patch path %q", path)
	}
	full := filepath.Join(root, path)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("patch outside management checkout: %s", path)
	}
	return full, nil
}

func invalidPatchPath(path string) bool {
	return path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." ||
		strings.HasPrefix(path, "..")
}

func validate(c config) error {
	if c.Upstream != "devsy-org/devsy" || c.UpstreamBranch != "main" {
		return errors.New("upstream must be devsy-org/devsy main")
	}
	seen := map[string]bool{}
	for _, b := range c.Branches {
		key := b.Repository + ":" + b.Branch
		if seen[key] {
			return fmt.Errorf("duplicate branch %s", key)
		}
		seen[key] = true
		if err := validateBranch(b); err != nil {
			return err
		}
	}
	return nil
}

func validateBranch(b branch) error {
	if !repoPattern.MatchString(b.Repository) {
		return errors.New("invalid repository")
	}
	if b.Branch == "" || strings.HasPrefix(b.Branch, "-") ||
		strings.ContainsAny(b.Branch, "\r\n\x00 ") {
		return errors.New("invalid branch")
	}
	if len(b.Checks) == 0 {
		return fmt.Errorf("branch %s requires checks", b.Branch)
	}
	if err := validateChecks(b.Checks); err != nil {
		return err
	}
	return validateResolution(b.Resolution)
}

func validateChecks(checks []string) error {
	for _, check := range checks {
		if check != "credential-relay" && check != "gh-relay" {
			return fmt.Errorf("unknown check %q; implement its release gate first", check)
		}
	}
	return nil
}

func validateResolution(res *resolution) error {
	if res == nil {
		return nil
	}
	if !shaPattern.MatchString(res.Before) || !shaPattern.MatchString(res.Incoming) {
		return errors.New("resolution must pin first-parent tree and incoming commit")
	}
	return nil
}

func resolve(r runner, dir, repo, ref string) (string, error) {
	value, err := r.git(
		dir,
		"ls-remote",
		"--exit-code",
		"https://github.com/"+repo+".git",
		"refs/heads/"+ref,
	)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/"+ref {
		return "", errors.New("branch did not resolve to exactly one commit")
	}
	r.observe(operation{Name: "resolve_ref", Status: "ok", Attributes: map[string]string{
		"repository": repo, "ref": "refs/heads/" + ref, "sha": fields[0],
	}})
	return fields[0], nil
}

func nextVersion(refs string) (string, error) {
	max := 0
	for line := range strings.SplitSeq(refs, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		matches := versionPattern.FindStringSubmatch(strings.TrimPrefix(fields[1], "refs/tags/"))
		if matches == nil {
			continue
		}
		n, err := strconv.Atoi(matches[1])
		if err != nil {
			return "", err
		}
		if n > max {
			max = n
		}
	}
	if max == int(^uint(0)>>1) {
		return "", errors.New("release sequence exhausted")
	}
	return fmt.Sprintf("v0.0.0-atty.%d", max+1), nil
}

func mergeBranch(r runner, source, manager string, b branch) error {
	before, err := r.git(source, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	_, mergeErr := r.git(
		source,
		"merge",
		"--no-ff",
		"--no-edit",
		"-m",
		"build(fork): integrate "+b.Repository+":"+b.Branch,
		b.SHA,
	)
	if mergeErr == nil {
		if b.Resolution != nil {
			return errors.New(
				"resolution is stale: merge no longer conflicts; remove it explicitly",
			)
		}
		return nil
	}
	unmerged, err := r.git(source, "diff", "--name-only", "--diff-filter=U")
	if err != nil || unmerged == "" {
		return mergeErr
	}
	if b.Resolution == nil {
		return fmt.Errorf(
			"merge conflict; before_commit=%s incoming=%s files=%s: %w",
			before,
			b.SHA,
			unmerged,
			mergeErr,
		)
	}
	return applyResolution(r, source, manager, b)
}

func applyResolution(r runner, source, manager string, b branch) error {
	before, err := r.git(source, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	res := b.Resolution
	beforeTree, treeErr := r.git(source, "rev-parse", "HEAD^{tree}")
	if treeErr != nil {
		return treeErr
	}
	if res.Before != beforeTree || res.Incoming != b.SHA {
		return fmt.Errorf(
			"stale conflict resolution: actual first-parent tree and incoming SHA %s %s",
			beforeTree,
			b.SHA,
		)
	}
	patch, err := safePath(manager, res.Patch)
	if err != nil {
		return err
	}
	if _, err = r.git(source, "merge", "--abort"); err != nil {
		return err
	}
	// A resolution is the complete binary diff from the first parent to the resolved merge tree.
	if _, err = r.git(source, "apply", "--index", "--whitespace=error", patch); err != nil {
		return err
	}
	return commitResolution(r, source, before, b)
}

func commitResolution(r runner, source, before string, b branch) error {
	tree, err := r.git(source, "write-tree")
	if err != nil {
		return err
	}
	commit, err := r.git(
		source,
		"commit-tree",
		"-S",
		tree,
		"-p",
		before,
		"-p",
		b.SHA,
		"-m",
		"build(fork): integrate "+b.Repository+":"+b.Branch+" with recorded resolution",
	)
	if err != nil {
		return err
	}
	_, err = r.git(source, "reset", "--hard", commit)
	return err
}

type assembly struct {
	r       runner
	manager string
	out     string
	source  string
	m       manifest
}

func prepare(r runner, manager, out, runURL string) error {
	a := assembly{r: r, manager: manager, out: out, source: filepath.Join(out, "source")}
	a.m.RunURL = runURL
	stages := []struct {
		name string
		run  func() error
	}{
		{"resolve inputs", a.resolveInputs},
		{"resolve selected branches", a.resolveBranches},
		{"record pinned inputs", a.recordInputs},
		{"create checkout", a.cloneSource},
		{"sign and assemble", a.assemble},
		{"record assembled source", a.recordSource},
	}
	for _, stage := range stages {
		if err := stage.run(); err != nil {
			return fmt.Errorf("%s: %w", stage.name, err)
		}
	}
	return nil
}

func cleanHead(r runner, dir string) (string, error) {
	dirty, err := r.git(dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", errors.New("checkout must be clean")
	}
	return r.git(dir, "rev-parse", "HEAD")
}

func (a *assembly) resolveInputs() error {
	if err := a.readConfig(); err != nil {
		return err
	}

	head, err := cleanHead(a.r, a.manager)
	if err != nil {
		return err
	}
	a.m.ManagementSHA = head
	refs, err := a.r.git(a.manager, "ls-remote", "--tags", "https://github.com/"+fork+".git")
	if err != nil {
		return err
	}
	a.m.Version, err = nextVersion(refs)
	if err != nil {
		return err
	}
	a.m.UpstreamSHA, err = resolve(a.r, a.manager, a.m.Config.Upstream, a.m.Config.UpstreamBranch)
	return err
}

func (a *assembly) readConfig() error {
	if err := readJSON(
		filepath.Join(a.manager, "hack/fork_release/config.json"),
		&a.m.Config,
	); err != nil {
		return fmt.Errorf("%w: %w", errConfigInvalid, err)
	}
	if err := validate(a.m.Config); err != nil {
		return fmt.Errorf("%w: %w", errConfigInvalid, err)
	}
	if err := writeJSON(filepath.Join(a.out, "input-config.json"), a.m.Config); err != nil {
		return err
	}
	if len(a.m.Config.Branches) == 0 {
		return errRetired
	}
	return nil
}

func (a *assembly) resolveBranches() error {
	for i := range a.m.Config.Branches {
		b := &a.m.Config.Branches[i]
		sha, err := resolve(a.r, a.manager, b.Repository, b.Branch)
		if err != nil {
			return err
		}
		b.SHA = sha
		for _, check := range b.Checks {
			if !slices.Contains(a.m.Checks, check) {
				a.m.Checks = append(a.m.Checks, check)
			}
		}
	}
	return nil
}

func (a *assembly) recordInputs() error {
	a.m.PatchHashes = map[string]string{}
	paths := slices.Clone(a.m.Config.Patches)
	for _, b := range a.m.Config.Branches {
		if b.Resolution != nil {
			paths = append(paths, b.Resolution.Patch)
		}
	}
	for _, path := range paths {
		if err := hashPatch(a.manager, path, &a.m); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(a.out, "resolved-inputs.json"), a.m)
}

func (a *assembly) cloneSource() error {
	if _, err := a.r.git(
		a.manager,
		"clone",
		"--no-hardlinks",
		"--no-checkout",
		a.manager,
		a.source,
	); err != nil {
		return err
	}
	pairs := [][2]string{
		{"user.name", "Devsy fork release"},
		{"user.email", "41898282+github-actions[bot]@users.noreply.github.com"},
		{"gpg.format", "ssh"},
		{"commit.gpgsign", "true"},
	}
	for _, pair := range pairs {
		if _, err := a.r.git(a.source, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	if _, err := a.r.git(
		a.source,
		"fetch",
		"--no-tags",
		"https://github.com/"+a.m.Config.Upstream+".git",
		a.m.UpstreamSHA,
	); err != nil {
		return err
	}
	_, err := a.r.git(a.source, "checkout", "--detach", a.m.UpstreamSHA)
	return err
}

func (a *assembly) assemble() error {
	key := filepath.Join(a.out, "signing-key")
	if _, err := a.r.run(
		a.manager,
		nil,
		"ssh-keygen",
		"-q",
		"-t",
		"ed25519",
		"-N",
		"",
		"-f",
		key,
	); err != nil {
		return err
	}
	defer func() { _ = os.Remove(key); _ = os.Remove(key + ".pub") }()
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	a.m.SigningPublicKey = strings.TrimSpace(string(pub))
	if _, err = a.r.git(a.source, "config", "user.signingkey", key); err != nil {
		return err
	}
	if err = a.mergeSelected(); err != nil {
		return err
	}
	return a.applyPatches()
}

func (a *assembly) mergeSelected() error {
	for _, b := range a.m.Config.Branches {
		if _, err := a.r.git(
			a.source,
			"fetch",
			"--no-tags",
			"https://github.com/"+b.Repository+".git",
			b.SHA,
		); err != nil {
			return err
		}
		if err := mergeBranch(a.r, a.source, a.manager, b); err != nil {
			return err
		}
	}
	return nil
}

func (a *assembly) applyPatches() error {
	for _, path := range a.m.Config.Patches {
		full, err := safePath(a.manager, path)
		if err != nil {
			return err
		}
		if _, err = a.r.git(a.source, "apply", "--index", "--whitespace=error", full); err != nil {
			return err
		}
	}
	if _, err := a.r.git(
		a.source,
		"rm",
		"--ignore-unmatch",
		".github/workflows/release.yml",
	); err != nil {
		return err
	}
	_, err := a.r.git(
		a.source,
		"commit",
		"-m",
		"build(fork): configure matched agent downloads and CLI-only release",
	)
	return err
}

func (a *assembly) recordSource() error {
	var err error
	a.m.SourceSHA, err = a.r.git(a.source, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(a.out, "build-info.json"), a.m); err != nil {
		return err
	}
	if _, err = a.r.git(
		a.source,
		"bundle",
		"create",
		filepath.Join(a.out, "source.bundle"),
		"HEAD",
	); err != nil {
		return err
	}
	return githubOutput(a.m)
}

func githubOutput(m manifest) error {
	output := os.Getenv("GITHUB_OUTPUT")
	if output == "" {
		return nil
	}
	f, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintf(
		f,
		"version=%s\nsource_sha=%s\nupstream_sha=%s\n",
		m.Version,
		m.SourceSHA,
		m.UpstreamSHA,
	)
	return err
}

func hashPatch(manager, path string, m *manifest) error {
	full, err := safePath(manager, path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	m.PatchHashes[path] = hex.EncodeToString(sum[:])
	return nil
}

func build(r runner, source, info, out string) error {
	var m manifest
	if err := readJSON(info, &m); err != nil {
		return err
	}
	if !versionPattern.MatchString(m.Version) || !shaPattern.MatchString(m.SourceSHA) {
		return errors.New("invalid build manifest")
	}
	head, err := cleanHead(r, source)
	if err != nil {
		return err
	}
	if head != m.SourceSHA {
		return errSourceMismatch
	}
	if runtime.GOOS != osLinux && runtime.GOOS != "darwin" {
		return errors.New("build requires Linux or macOS")
	}
	return buildArchitectures(r, source, out, m)
}

func buildArchitectures(r runner, source, out string, m manifest) error {
	flags := strings.Join([]string{
		"-s -w",
		"-X main.build=" + m.Version,
		"-X main.commit=" + m.SourceSHA,
		"-X github.com/devsy-org/devsy/pkg/version.version=" + m.Version,
		"-X github.com/devsy-org/devsy/pkg/config.forkAgentDownloadBaseURL=" +
			"https://github.com/" + fork + "/releases/download/",
	}, " ")
	if _, err := r.run(
		source,
		[]string{"DEVSY_FORK_TEST_VERSION=" + m.Version},
		"go",
		"test",
		"-tags",
		"forkrelease",
		"-ldflags",
		flags,
		"./pkg/config",
		"./pkg/options",
		"./pkg/agent",
	); err != nil {
		return err
	}
	for _, arch := range []string{"amd64", "arm64"} {
		binary := filepath.Join(out, "devsy-"+runtime.GOOS+"-"+arch)
		args := []string{commandBuild, "-trimpath", "-ldflags", flags, "-o", binary, "."}
		if runtime.GOOS == osLinux {
			args = append([]string{commandBuild, "-tags", "netgo,osusergo"}, args[1:]...)
		}
		if _, err := r.run(
			source,
			[]string{"CGO_ENABLED=0", "GOOS=" + runtime.GOOS, "GOARCH=" + arch},
			"go",
			args...); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	flags := flag.NewFlagSet("fork-release", flag.ContinueOnError)
	root := flags.String("root", ".", "management checkout")
	out := flags.String("out", "", "assembly or build output")
	source := flags.String("source", "", "assembled checkout, or assembly directory for publish")
	info := flags.String("info", "", "manifest, or downloaded artifact directory for publish")
	runURL := flags.String("run-url", "", "Actions run URL")
	if len(args) == 0 {
		return errors.New("expected prepare, build or publish")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	manager, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	output, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err = createOutput(args[0], output); err != nil {
		return err
	}
	record, closeRecord := openRecording(output)
	defer closeRecord()
	r := runner{record: record}
	return recordedDispatch(
		assembly{r: r, manager: manager, out: output},
		args[0],
		[]string{*source, *info, *runURL},
	)
}

func createOutput(command, path string) error {
	if command == "prepare" {
		return os.Mkdir(path, 0o700)
	}
	return os.MkdirAll(path, 0o700)
}

func openRecording(output string) (io.Writer, func()) {
	noop := func() {}
	if os.Getenv("DEVSY_FORK_RECORDING") == "0" {
		return nil, noop
	}
	f, err := os.OpenFile(
		filepath.Join(output, "operations.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o600,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "operation recording unavailable:", err)
		return nil, noop
	}
	return &boundedRecord{writer: f, remaining: 4 << 20}, func() { _ = f.Close() }
}

func dispatch(a assembly, command string, values []string) error {
	switch command {
	case "prepare":
		return prepare(a.r, a.manager, a.out, values[2])
	case commandBuild:
		return build(a.r, values[0], values[1], a.out)
	case "publish":
		return publish(a.r, values[0], values[1], a.out)
	default:
		return errors.New("expected prepare, build or publish")
	}
}
