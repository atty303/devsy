package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type releaseAssets struct {
	out   string
	paths []string
	sums  strings.Builder
}

func (a *releaseAssets) add(name string, data []byte, mode os.FileMode) error {
	path := filepath.Join(a.out, name)
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	fmt.Fprintf(&a.sums, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	a.paths = append(a.paths, path)
	return nil
}

func (a *releaseAssets) copy(root, name string, mode os.FileMode) error {
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("empty asset %s", name)
	}
	return a.add(name, data, mode)
}

func (a *releaseAssets) binaries(downloads string) error {
	for _, target := range []struct{ os, artifact string }{{"darwin", "macOS"}, {osLinux, "Linux"}} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := "devsy-" + target.os + "-" + arch
			if err := a.copy(
				filepath.Join(downloads, "binaries-"+target.artifact),
				name,
				0o755,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *releaseAssets) metadata(source, assembly string) error {
	for _, name := range []string{"LICENSE", "THIRD_PARTY_LICENSES.md"} {
		if err := a.copy(source, name, 0o644); err != nil {
			return err
		}
	}
	if err := a.copy(assembly, "build-info.json", 0o644); err != nil {
		return err
	}
	path := filepath.Join(a.out, "checksums.txt")
	if err := os.WriteFile(path, []byte(a.sums.String()), 0o644); err != nil {
		return err
	}
	a.paths = append(a.paths, path)
	return nil
}

func releaseNotes(m manifest) string {
	var body strings.Builder
	fmt.Fprintf(
		&body,
		"CLI and Linux agent built from `%s`.\n\nUpstream: `%s`\nManagement: `%s`\n\n",
		m.SourceSHA,
		m.UpstreamSHA,
		m.ManagementSHA,
	)
	for _, b := range m.Config.Branches {
		fmt.Fprintf(&body, "- `%s:%s` at `%s`\n", b.Repository, b.Branch, b.SHA)
	}
	body.WriteString(`
The CLI defaults to the agent from this same fork release. Explicit DEVSY_AGENT_URL
or context AGENT_URL overrides retain precedence; remove old overrides when switching versions.

Install with mise's github:atty303/devsy backend (latest or an explicit version).
Desktop and Windows are not included.

Required Linux/macOS unit tests, CLI CI lint, selected PR E2Es and four binary builds
passed before publication. Full source selection and patch hashes are in build-info.json.

Workflow: `)
	body.WriteString(m.RunURL + "\n")
	return body.String()
}

func publicationManifest(path string) (manifest, error) {
	var m manifest
	if err := readJSON(path, &m); err != nil {
		return m, err
	}
	valid := versionPattern.MatchString(m.Version) && shaPattern.MatchString(m.SourceSHA) &&
		shaPattern.MatchString(m.ManagementSHA)
	if !valid {
		return m, errors.New("invalid release manifest")
	}
	return m, nil
}

// publish only accepts complete artifacts from the workflow's required jobs.
func publish(r runner, assembly, downloads, out string) error {
	m, err := publicationManifest(filepath.Join(assembly, "build-info.json"))
	if err != nil {
		return err
	}
	source := filepath.Join(out, "source")
	if _, err = r.git(out, "clone", filepath.Join(assembly, "source.bundle"), source); err != nil {
		return err
	}
	head, err := r.git(source, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != m.SourceSHA {
		return errSourceMismatch
	}
	assets := releaseAssets{out: out}
	if err = assets.binaries(downloads); err != nil {
		return err
	}
	if err = assets.metadata(source, assembly); err != nil {
		return err
	}
	return createRelease(r, source, &assets, m)
}

func createRelease(r runner, source string, assets *releaseAssets, m manifest) error {
	notes := filepath.Join(assets.out, "notes.md")
	if err := os.WriteFile(notes, []byte(releaseNotes(m)), 0o600); err != nil {
		return err
	}
	// A new immutable tag reserves the version. An interrupted publication is never overwritten.
	if _, err := r.git(source, "-c", "credential.helper=!gh auth git-credential", "push",
		"https://github.com/"+fork+".git", m.SourceSHA+":refs/tags/"+m.Version); err != nil {
		return err
	}
	args := []string{
		"release", "create", m.Version, "--repo", fork, "--verify-tag", "--draft",
		"--title", m.Version, "--notes-file", notes,
	}
	args = append(args, assets.paths...)
	if _, err := r.run(assets.out, nil, "gh", args...); err != nil {
		return err
	}
	if err := verifyDraft(r, assets, m.Version); err != nil {
		return err
	}
	_, err := r.run(assets.out, nil, "gh", "release", "edit", m.Version, "--repo", fork,
		"--draft=false", "--prerelease=false", "--latest")
	return err
}

func verifyDraft(r runner, assets *releaseAssets, version string) error {
	response, err := r.run(assets.out, nil, "gh", "release", "view", version, "--repo", fork,
		"--json", "assets", "--jq", ".assets[].name")
	if err != nil {
		return err
	}
	names := strings.Fields(string(response))
	if len(names) != len(assets.paths) {
		return errors.New("draft asset count mismatch; draft retained without publishing")
	}
	for _, asset := range assets.paths {
		if !slices.Contains(names, filepath.Base(asset)) {
			return fmt.Errorf("draft missing asset %s", filepath.Base(asset))
		}
	}
	return nil
}
