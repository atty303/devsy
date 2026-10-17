package agentcontainer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/devsy-org/devsy/cmd/flags"
	"github.com/devsy-org/devsy/pkg/agent/tunnel"
	"github.com/devsy-org/devsy/pkg/agent/tunnelserver"
	"github.com/devsy-org/devsy/pkg/config"
	"github.com/devsy-org/devsy/pkg/credentials"
	devconfig "github.com/devsy-org/devsy/pkg/devcontainer/config"
	"github.com/devsy-org/devsy/pkg/dockercredentials"
	cliflags "github.com/devsy-org/devsy/pkg/flags"
	"github.com/devsy-org/devsy/pkg/flags/names"
	"github.com/devsy-org/devsy/pkg/gitcredentials"
	"github.com/devsy-org/devsy/pkg/gitsshsigning"
	"github.com/devsy-org/devsy/pkg/log"
	"github.com/devsy-org/devsy/pkg/netstat"
	"github.com/spf13/cobra"
)

// CredentialsServerCmd holds the cmd flags.
type CredentialsServerCmd struct {
	*flags.GlobalFlags

	User string

	ConfigureGitHelper    bool
	ConfigureDockerHelper bool

	ForwardPorts      bool
	GitUserSigningKey string
}

// NewCredentialsServerCmd creates a new command.
func NewCredentialsServerCmd(flags *flags.GlobalFlags) *cobra.Command {
	cmd := &CredentialsServerCmd{
		GlobalFlags: flags,
	}
	credentialsServerCmd := &cobra.Command{
		Use:   "credentials-server",
		Short: "Starts a credentials server",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			port, err := credentials.GetPort()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return cmd.Run(ctx, port)
		},
	}
	cliflags.Add(
		credentialsServerCmd,
		cliflags.Bool(
			&cmd.ConfigureGitHelper,
			names.ConfigureGitHelper,
			false,
			"If true will configure git helper",
		),
		cliflags.Bool(
			&cmd.ConfigureDockerHelper,
			names.ConfigureDockerHelper,
			false,
			"If true will configure docker helper",
		),
		cliflags.Bool(
			&cmd.ForwardPorts,
			names.ForwardPorts,
			false,
			"If true will automatically try to forward open ports within the container",
		),
		cliflags.String(&cmd.GitUserSigningKey, names.GitUserSigningKey, "", ""),
		cliflags.String(&cmd.User, names.User, "", "The user to use"),
	)
	_ = credentialsServerCmd.MarkFlagRequired(names.User)

	return credentialsServerCmd
}

// Run runs the command logic.
func (cmd *CredentialsServerCmd) Run(ctx context.Context, port int) (runErr error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer func() {
		log.Infof("credentials services ended: user=%s port=%d pid=%d error=%v cause=%v",
			cmd.User, port, os.Getpid(), runErr, context.Cause(runCtx))
	}()

	// Establish RPC before waiting for the port. Reading the tunnel's EOF also
	// cancels an idle contender whose SSH session has disconnected.
	tunnelClient, err := connectCredentialsTunnel(runCtx, cancel)
	if err != nil {
		if runCtx.Err() != nil {
			return nil
		}
		return err
	}
	ln, err := cmd.waitForPort(runCtx, port)
	if err != nil {
		if runCtx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { _ = ln.Close() }()

	cmd.maybeForwardPorts(runCtx, tunnelClient)

	if err := cmd.configureDockerHelper(port); err != nil {
		return err
	}

	if err := configureGitUserLocally(runCtx, cmd.User, tunnelClient); err != nil {
		log.Warnf("error configuring git user: %v", err)
		return err
	}

	cleanupGitHelper, err := cmd.configureGitCredentialHelper(runCtx, port)
	if err != nil {
		return err
	}
	cleanupGitSigning := cmd.configureGitSigningKey()
	// HTTP server shutdown closes its listener before returning. Keep helper
	// cleanup inside Close, so a successor cannot acquire the port too early.
	ln = &credentialsListener{Listener: ln, cleanup: func() {
		cleanupGitSigning()
		cleanupGitHelper()
		log.Infof(
			"credentials helper cleanup completed: user=%s port=%d pid=%d",
			cmd.User,
			port,
			os.Getpid(),
		)
	}}

	return credentials.RunCredentialsServerWithListener(runCtx, ln, tunnelClient, cmd.User)
}

func connectCredentialsTunnel(
	ctx context.Context,
	cancel context.CancelCauseFunc,
) (tunnel.TunnelClient, error) {
	client, err := tunnelserver.NewTunnelClientWithContext(ctx,
		&credentialsTunnelReader{Reader: os.Stdin, cancel: cancel}, os.Stdout)
	if err != nil {
		return nil, fmt.Errorf("error creating tunnel client: %w", err)
	}
	if _, err := client.Ping(ctx, &tunnel.Empty{}); err != nil {
		return nil, fmt.Errorf("ping client: %w", err)
	}
	return client, nil
}

type credentialsTunnelReader struct {
	io.Reader
	cancel context.CancelCauseFunc
}

func (r *credentialsTunnelReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.cancel(err)
	}
	return n, err
}

type credentialsListener struct {
	net.Listener
	cleanup func()
	once    sync.Once
	err     error
}

func (l *credentialsListener) Close() error {
	l.once.Do(func() {
		l.cleanup()
		l.err = l.Listener.Close()
	})
	return l.err
}

func (cmd *CredentialsServerCmd) waitForPort(ctx context.Context, port int) (net.Listener, error) {
	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ln, err := claimPort(port)
		if err == nil {
			log.Infof(
				"credentials port acquired: user=%s port=%d pid=%d",
				cmd.User,
				port,
				os.Getpid(),
			)
			return ln, nil
		}
		if !errors.Is(err, errPortOwnedByAnotherSession) {
			return nil, err
		}
		if !waiting {
			log.Infof(
				"waiting for credentials port: user=%s port=%d pid=%d",
				cmd.User,
				port,
				os.Getpid(),
			)
			cmd.logPortOwnedByAnotherSession(ctx, port)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// errPortOwnedByAnotherSession marks a bind failure as another session
// already owning the port, not a real error.
var errPortOwnedByAnotherSession = errors.New("credentials server port owned by another session")

// claimPort binds port and returns the listener, holding it exclusively so
// no other session can bind the same port until the caller closes it (or
// hands it to RunCredentialsServerWithListener). Only one session's
// credentials-server can hold this port at a time.
func claimPort(port int) (net.Listener, error) {
	addr := net.JoinHostPort("localhost", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w: %w", errPortOwnedByAnotherSession, err)
		}
		return nil, fmt.Errorf("port %d not available: %w", port, err)
	}
	return ln, nil
}

func (cmd *CredentialsServerCmd) logPortOwnedByAnotherSession(ctx context.Context, port int) {
	owner, err := credentials.FetchOwner(ctx, port)
	switch {
	case err != nil:
		log.Debugf(
			"waiting to start credentials server for user %s: port %d is taken and its owner could not be determined: %v",
			cmd.User,
			port,
			err,
		)
	case owner == "" || owner == cmd.User:
		log.Debugf(
			"waiting to start credentials server for user %s: another session already provides it on port %d",
			cmd.User,
			port,
		)
	default:
		log.Warnf(
			"credentials server for user %s is waiting: port %d is already owned by user %s's session; "+
				"git/docker/signing credential helpers for %s will not work until that session ends",
			cmd.User,
			port,
			owner,
			cmd.User,
		)
	}
}

func (cmd *CredentialsServerCmd) maybeForwardPorts(
	ctx context.Context,
	tunnelClient tunnel.TunnelClient,
) {
	if !cmd.ForwardPorts {
		return
	}
	go func() {
		log.Debugf("start watching & forwarding open ports")
		if err := forwardPorts(ctx, tunnelClient); err != nil {
			log.Errorf("error forwarding ports: %v", err)
		}
	}()
}

func (cmd *CredentialsServerCmd) configureDockerHelper(port int) error {
	if !cmd.ConfigureDockerHelper {
		return nil
	}
	return dockercredentials.ConfigureCredentialsContainer(cmd.User, port)
}

func (cmd *CredentialsServerCmd) configureGitCredentialHelper(
	ctx context.Context,
	port int,
) (func(), error) {
	noop := func() {}
	if !cmd.ConfigureGitHelper {
		return noop, nil
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return noop, err
	}
	if err := gitcredentials.ConfigureHelper(ctx, binaryPath, cmd.User, port); err != nil {
		return noop, fmt.Errorf("configure git helper: %w", err)
	}

	cleanupCtx := context.WithoutCancel(ctx)
	userName := cmd.User
	return func() {
		_ = gitcredentials.RemoveHelper(cleanupCtx, userName)
	}, nil
}

func (cmd *CredentialsServerCmd) configureGitSigningKey() func() {
	noop := func() {}
	if cmd.GitUserSigningKey == "" {
		return noop
	}

	decodedKey, err := base64.StdEncoding.DecodeString(cmd.GitUserSigningKey)
	if err != nil {
		log.Errorf("failed to decode git SSH signing key, signing will be unavailable: %v", err)
		return noop
	}

	if err := gitsshsigning.ConfigureHelper(cmd.User, string(decodedKey)); err != nil {
		log.Errorf(
			"failed to configure git SSH signature helper, signing will be unavailable: %v",
			err,
		)
		return noop
	}

	userName := cmd.User
	return func() {
		_ = gitsshsigning.RemoveHelper(userName)
	}
}

func configureGitUserLocally(
	ctx context.Context,
	userName string,
	client tunnel.TunnelClient,
) error {
	localGitUser, err := gitcredentials.GetUser(ctx, userName, "")
	if err != nil {
		return err
	}
	if localGitUser.Name != "" && localGitUser.Email != "" {
		return nil
	}

	gitUser, err := fetchRemoteGitUser(ctx, client)
	if err != nil {
		return err
	}

	clearKnownGitUserFields(localGitUser, gitUser)

	if err := gitcredentials.SetUser(ctx, userName, gitUser); err != nil {
		return fmt.Errorf("set git user & email: %w", err)
	}

	return nil
}

func fetchRemoteGitUser(
	ctx context.Context,
	client tunnel.TunnelClient,
) (*gitcredentials.GitUser, error) {
	response, err := client.GitUser(ctx, &tunnel.Empty{})
	if err != nil {
		return nil, fmt.Errorf("retrieve git user: %w", err)
	}

	gitUser := &gitcredentials.GitUser{}
	if err := json.Unmarshal([]byte(response.Message), gitUser); err != nil {
		return nil, fmt.Errorf("decode git user: %w", err)
	}

	return gitUser, nil
}

func clearKnownGitUserFields(local, remote *gitcredentials.GitUser) {
	if local.Name != "" {
		remote.Name = ""
	}
	if local.Email != "" {
		remote.Email = ""
	}
}

func forwardPorts(ctx context.Context, client tunnel.TunnelClient) error {
	opts := portOptionsFromResult()
	return netstat.NewWatcher(&forwarder{ctx: ctx, client: client}, opts...).Run(ctx)
}

func portOptionsFromResult() []netstat.WatcherOption {
	raw, err := os.ReadFile(config.DevContainerResultPath)
	if err != nil {
		log.Debugf("could not read result for port attributes: %v", err)
		return nil
	}
	result := &devconfig.Result{}
	if err := json.Unmarshal(raw, result); err != nil {
		log.Debugf("could not parse result for port attributes: %v", err)
		return nil
	}
	mc := result.MergedConfig
	if mc == nil || (len(mc.PortsAttributes) == 0 && mc.OtherPortsAttributes == nil) {
		return nil
	}
	pa, opa := mc.PortsAttributes, mc.OtherPortsAttributes
	resolver := func(port string) netstat.PortForwardAttribute {
		portNum, err := strconv.Atoi(port)
		if err != nil {
			return netstat.PortForwardAttribute{}
		}
		attr := devconfig.ResolvePortAttribute(portNum, pa, opa)
		return netstat.PortForwardAttribute{
			Label:            attr.Label,
			Protocol:         attr.Protocol,
			OnAutoForward:    attr.OnAutoForward,
			RequireLocalPort: attr.RequireLocalPort,
			ElevateIfNeeded:  attr.ElevateIfNeeded,
		}
	}
	return []netstat.WatcherOption{
		netstat.WithPortAttributes(resolver),
	}
}

type forwarder struct {
	ctx context.Context

	client tunnel.TunnelClient
}

// Forward relays the auto-discovered port to the client over gRPC.
func (f *forwarder) Forward(port string, attr netstat.PortForwardAttribute) error {
	if attr.Label != "" {
		log.Debugf("forwarding port %s (%s, protocol=%s)", port, attr.Label, attr.Protocol)
	}
	_, err := f.client.ForwardPort(f.ctx, &tunnel.ForwardPortRequest{Port: port})
	return err
}

func (f *forwarder) StopForward(port string) error {
	_, err := f.client.StopForwardPort(f.ctx, &tunnel.StopForwardPortRequest{Port: port})
	return err
}
