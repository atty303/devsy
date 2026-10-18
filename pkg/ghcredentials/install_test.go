package ghcredentials

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testOriginalName  = "real-gh"
	testExitScript    = "#!/bin/sh\nexit 0"
	testRegularFile   = "file"
	testRelativeLink  = "relative symlink"
	testInstallBinary = "/devsy"
)

func writeTestExecutable(t *testing.T, path string, data []byte) {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Dir(path))
	require.NoError(t, err)
	defer func() { _ = dir.Close() }()
	require.NoError(t, dir.WriteFile(filepath.Base(path), data, 0o700))
}

func TestInstallWrapperPreservesOriginalAndUpdates(t *testing.T) {
	for _, kind := range []string{testRegularFile, "absolute symlink", testRelativeLink} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "gh")
			target := filepath.Join(dir, testOriginalName)
			first := []byte("#!/bin/sh\nprintf first")
			writeTestExecutable(t, target, first)
			switch kind {
			case testRegularFile:
				writeTestExecutable(t, path, first)
			case "absolute symlink":
				require.NoError(t, os.Symlink(target, path))
			case testRelativeLink:
				require.NoError(t, os.Symlink(testOriginalName, path))
			}
			require.NoError(t, installWrapper(path, "/devsy first", 12049))
			backup := path + backupSuffix
			original, err := readFile(backup)
			require.NoError(t, err)
			require.Equal(t, first, original)
			if kind != testRegularFile {
				link, err := os.Readlink(backup)
				require.NoError(t, err)
				if kind == testRelativeLink {
					require.Equal(t, testOriginalName, link)
				} else {
					require.Equal(t, target, link)
				}
			}
			require.NoError(t, installWrapper(path, "/devsy second", 12050))
			original, err = readFile(backup)
			require.NoError(t, err)
			require.Equal(t, first, original)
			script, err := readFile(path)
			require.NoError(t, err)
			require.Contains(t, string(script), "12050")
			require.Contains(t, string(script), "'/devsy second'")
			require.NoError(t, os.Remove(path))
			second := []byte("#!/bin/sh\nprintf second")
			writeTestExecutable(t, path, second)
			require.NoError(t, installWrapper(path, testInstallBinary, 12049))
			original, err = readFile(backup)
			require.NoError(t, err)
			require.Equal(t, second, original)
		})
	}
}

func TestInstallWrapperWithoutOriginalAndInterruptedMove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	require.NoError(t, installWrapper(path, testInstallBinary, 12049))
	_, err := os.Lstat(path + backupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.Remove(path))
	original := []byte("#!/bin/sh\nprintf preserved")
	writeTestExecutable(t, path+backupSuffix, original)
	require.NoError(t, installWrapper(path, testInstallBinary, 12049))
	got, err := readFile(path + backupSuffix)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestInstallWrapperConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	original := []byte("#!/bin/sh\nprintf original")
	writeTestExecutable(t, path, original)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- installWrapper(path, testInstallBinary, 12049) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := readFile(path + backupSuffix)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestInstallWrapperFailurePreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	original := []byte("#!/bin/sh\nprintf original")
	writeTestExecutable(t, path, original)
	require.NoError(t, os.Mkdir(path+backupSuffix, 0o700))
	require.Error(t, installWrapper(path, testInstallBinary, 12049))
	got, err := readFile(path)
	require.NoError(t, err)
	require.Equal(t, original, got)
	entries, err := filepath.Glob(filepath.Join(dir, ".devsy-gh-wrapper-*"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRealBinaryRespectsPATHAndBackup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	early := filepath.Join(root, "early")
	standard := filepath.Join(root, "standard")
	late := filepath.Join(root, "late")
	for _, dir := range []string{early, standard, late} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	wrapper := filepath.Join(standard, "gh")
	require.NoError(t, installWrapper(wrapper, testInstallBinary, 12049))
	later := filepath.Join(late, "gh")
	writeTestExecutable(t, later, []byte(testExitScript))
	t.Setenv("PATH", standard+string(os.PathListSeparator)+late)
	got, err := realBinary(wrapper)
	require.NoError(t, err)
	require.Equal(t, later, got)
	backup := wrapper + backupSuffix
	writeTestExecutable(t, backup, []byte(testExitScript))
	got, err = realBinary(wrapper)
	require.NoError(t, err)
	require.Equal(t, backup, got)
	selected := filepath.Join(early, "gh")
	writeTestExecutable(t, selected, []byte(testExitScript))
	t.Setenv("PATH", early+string(os.PathListSeparator)+standard+string(os.PathListSeparator)+late)
	got, err = realBinary(wrapper)
	require.NoError(t, err)
	require.Equal(t, selected, got)
	require.NoError(t, os.Remove(selected))
	require.NoError(t, os.Remove(backup))
	require.NoError(t, os.Link(wrapper, backup))
	got, err = realBinary(wrapper)
	require.NoError(t, err)
	require.Equal(t, later, got)
}
