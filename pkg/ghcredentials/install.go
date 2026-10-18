package ghcredentials

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/devsy-org/devsy/pkg/envfile"
)

const WrapperDir = "/usr/local/share/devsy/gh/bin"

func Install(port int) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	// #nosec G301 -- Public executable path; it contains no authentication data.
	if err := os.MkdirAll(WrapperDir, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf(
		"#!/bin/sh\n# devsy gh relay\nexport DEVSY_CREDENTIALS_SERVER_PORT=%d\nexec %s internal gh \"$0\" \"$@\"\n",
		port,
		shellescape.Quote(binary),
	)
	//nolint:gosec // This is the executable wrapper, containing no credentials.
	if err := os.WriteFile(filepath.Join(WrapperDir, "gh"), []byte(script), 0o755); err != nil {
		return err
	}
	// Devsy SSH/IDE sessions read envfile; login shells also need the wrapper.
	path := os.Getenv("PATH")
	if !strings.HasPrefix(path, WrapperDir+":") {
		path = WrapperDir + ":" + path
	}
	envfile.MergeAndApply(map[string]string{"PATH": path})
	// #nosec G301 -- System shell profiles must be readable by workspace users.
	if err := os.MkdirAll("/etc/profile.d", 0o755); err != nil {
		return err
	}
	profile := fmt.Sprintf(`# devsy gh relay
case ":$PATH:" in *:%s:*) ;; *) export PATH=%s:"$PATH" ;; esac
`, WrapperDir, WrapperDir)
	// #nosec G306 -- Public PATH configuration, containing no credentials.
	return os.WriteFile("/etc/profile.d/devsy-gh.sh", []byte(profile), 0o644)
}
