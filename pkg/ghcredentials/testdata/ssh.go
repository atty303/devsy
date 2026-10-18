package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/crypto/ssh"
)

func main() {
	if err := run(); err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitStatus())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	config := &ssh.ClientConfig{User: "root", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second} //nolint:gosec // Network-disabled disposable fixture server.
	var client *ssh.Client
	var err error
	for range 50 {
		client, err = ssh.Dial("tcp", "127.0.0.1:12050", config)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	for _, name := range []string{"HOME", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_PROMPT_DISABLED", "GH_BROWSER", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "DBUS_SESSION_BUS_ADDRESS", "DEVSY_GH_DIAGNOSTICS"} {
		if value, ok := os.LookupEnv(name); ok {
			if err := session.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	session.Stdout, session.Stderr = os.Stdout, os.Stderr
	if len(os.Args) == 2 && os.Args[1] == "--probe" {
		return session.Run("command -v gh")
	}
	session.Stdin = os.Stdin
	return session.Run(shellescape.QuoteCommand(append([]string{"gh"}, os.Args[1:]...)))
}
