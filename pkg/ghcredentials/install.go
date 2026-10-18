package ghcredentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"al.essio.dev/pkg/shellescape"
)

const (
	wrapperPath   = "/usr/local/bin/gh"
	backupSuffix  = ".devsy-original"
	wrapperHeader = "#!/bin/sh\n# devsy gh relay\n"
)

func Install(port int) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	return installWrapper(wrapperPath, binary, port)
}

func installWrapper(path, binary string, port int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := filepath.Dir(path)
	// #nosec G301 -- Public executable directory without authentication data.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	l, err := lock(ctx, filepath.Join(dir, ".devsy-gh-install.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	staged, err := stageWrapper(dir, binary, port)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	moved, err := backupOriginal(path)
	if err != nil {
		return err
	}
	if err := os.Rename(staged, path); err != nil {
		if moved {
			return errors.Join(err, os.Rename(path+backupSuffix, path))
		}
		return err
	}
	return nil
}

func stageWrapper(dir, binary string, port int) (name string, err error) {
	script := fmt.Sprintf(
		wrapperHeader+"export DEVSY_CREDENTIALS_SERVER_PORT=%d\nexec %s internal gh \"$0\" \"$@\"\n",
		port,
		shellescape.Quote(binary),
	)
	f, err := os.CreateTemp(dir, ".devsy-gh-wrapper-*")
	if err != nil {
		return "", err
	}
	name = f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = f.WriteString(script); err != nil {
		return name, err
	}
	if err = f.Chmod(0o755); err != nil {
		return name, err
	}
	err = f.Close()
	return name, err
}

func backupOriginal(path string) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	own, err := isWrapper(path)
	if err != nil || own {
		return false, err
	}
	if _, err := executableInfo(path); err != nil {
		return false, err
	}
	if err := os.Rename(path, path+backupSuffix); err != nil {
		return false, err
	}
	return true, nil
}

func isWrapper(path string) (bool, error) {
	f, err := resolvedFile(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	header := make([]byte, len(wrapperHeader))
	_, err = io.ReadFull(f, header)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(header) == wrapperHeader, nil
}
