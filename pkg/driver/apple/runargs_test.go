package apple

import (
	"strings"
	"testing"

	"github.com/devsy-org/devsy/pkg/devcontainer/config"
	"github.com/devsy-org/devsy/pkg/driver"
	"github.com/devsy-org/devsy/pkg/types"
	"github.com/stretchr/testify/require"
)

const testTmpfsTarget = "/scratch"

func TestBuildRunArgs(t *testing.T) {
	d := &appleDriver{IDLabels: []string{"dev.containers.id"}}
	init := true
	params := &driver.RunImageDevContainerParams{
		WorkspaceID: "ws",
		ParsedConfig: &config.DevContainerConfig{
			NonComposeBase: config.NonComposeBase{
				AppPort: types.StrIntArray{"8080"},
			},
		},
		Options: &driver.RunOptions{
			Image:      testImageRef,
			User:       "vscode",
			Env:        map[string]string{"FOO": "bar"},
			Init:       &init,
			CapAdd:     []string{"SYS_PTRACE"},
			Entrypoint: "/bin/sh",
			Cmd:        []string{"-c", "sleep infinity"},
		},
	}

	args, err := d.buildRunArgs(params)
	require.NoError(t, err)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run -d",
		"-p 8080:8080",
		"-u vscode",
		"-e FOO=bar",
		"--init",
		"--cap-add SYS_PTRACE",
		"--entrypoint /bin/sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\n got: %s", want, joined)
		}
	}

	// Image must precede the command, and both must be last.
	if args[len(args)-3] != testImageRef {
		t.Errorf("expected image before cmd, got tail: %v", args[len(args)-3:])
	}

	// Apple does not support these Docker/Podman flags.
	for _, forbidden := range []string{"--security-opt", "--gpus", "--userns", "--sig-proxy"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("args contains unsupported flag %q: %s", forbidden, joined)
		}
	}
}

func TestBuildRunArgsRosetta(t *testing.T) {
	params := &driver.RunImageDevContainerParams{
		ParsedConfig: &config.DevContainerConfig{},
		Options:      &driver.RunOptions{Image: testImageRef},
	}

	off, err := (&appleDriver{}).buildRunArgs(params)
	require.NoError(t, err)
	if strings.Contains(strings.Join(off, " "), "--rosetta") {
		t.Errorf("--rosetta must not be present when Rosetta is disabled: %v", off)
	}

	on, err := (&appleDriver{Rosetta: true}).buildRunArgs(params)
	require.NoError(t, err)
	if !strings.Contains(strings.Join(on, " "), "--rosetta") {
		t.Errorf("--rosetta must be present when Rosetta is enabled: %v", on)
	}
}

func TestCleanMount(t *testing.T) {
	in := "type=bind,source=/a,target=/b,consistency=cached,bind-create-src=true"
	got := cleanMount(in)
	if strings.Contains(got, "consistency=") || strings.Contains(got, "bind-create-src=") {
		t.Errorf("cleanMount left docker-desktop options: %q", got)
	}
	for _, want := range []string{"type=bind", "source=/a", "target=/b"} {
		if !strings.Contains(got, want) {
			t.Errorf("cleanMount dropped %q: %q", want, got)
		}
	}
}

func TestRedactArgs(t *testing.T) {
	args := []string{
		"-e", "TOKEN=supersecret", "--build-arg", "NPM_AUTH=abc123",
		"-l", "keep=visible", "alpine",
	}
	got := redactArgs(args)
	if strings.Contains(got, "supersecret") || strings.Contains(got, "abc123") {
		t.Errorf("redactArgs leaked a secret: %s", got)
	}
	for _, want := range []string{"TOKEN=****", "NPM_AUTH=****", "keep=visible"} {
		if !strings.Contains(got, want) {
			t.Errorf("redactArgs = %q, missing %q", got, want)
		}
	}
}

func TestBuildRunArgsTmpfs(t *testing.T) {
	tests := tmpfsRunArgsCases()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mount := config.ParseMount(tt.mount)
			params := &driver.RunImageDevContainerParams{
				ParsedConfig: &config.DevContainerConfig{},
				Options: &driver.RunOptions{
					Image:  testImageRef,
					Mounts: []*config.Mount{&mount},
				},
			}
			args, err := (&appleDriver{}).buildRunArgs(params)
			require.NoError(t, err)
			require.Equal(
				t,
				[]string{"--tmpfs", tt.want},
				args[2:4],
			)
		})
	}
}

func tmpfsRunArgsCases() []struct{ name, mount, want string } {
	return []struct{ name, mount, want string }{
		{
			"secret files",
			"type=tmpfs,target=/run/secrets,tmpfs-mode=0755",
			"/run/secrets:mode=755",
		},
		{
			"secret environment",
			"type=tmpfs,target=/run/devsy/secrets-env,tmpfs-mode=0700",
			"/run/devsy/secrets-env:mode=700",
		},
		{"size below MiB", "type=tmpfs,target=/scratch,tmpfs-size=4096", "/scratch:size=4096"},
		{
			"size not whole MiB",
			"type=tmpfs,target=/scratch,tmpfs-size=1052672",
			"/scratch:size=1052672",
		},
		{
			"combined",
			"type=tmpfs,target=/scratch,tmpfs-size=1052672,tmpfs-mode=1770,readonly",
			"/scratch:size=1052672,mode=1770,ro",
		},
		{"ro alias", "type=tmpfs,target=/scratch,ro", testTmpfsTarget + ":ro"},
		{"Docker short true", "type=tmpfs,target=/scratch,readonly=t", testTmpfsTarget + ":ro"},
		{"Docker uppercase true", "type=tmpfs,target=/scratch,readonly=T", testTmpfsTarget + ":ro"},
		{
			"last mode wins",
			"type=tmpfs,target=/scratch,tmpfs-mode=0777,tmpfs-mode=0700",
			"/scratch:mode=700",
		},
		{
			"last size wins",
			"type=tmpfs,target=/scratch,tmpfs-size=1048576,tmpfs-size=4096",
			"/scratch:size=4096",
		},
		{"last readonly wins", "type=tmpfs,target=/scratch,readonly,ro=false", testTmpfsTarget},
		{"writable", "type=tmpfs,target=/scratch,readonly=false", testTmpfsTarget},
		{"defaults", "type=tmpfs,target=/scratch", testTmpfsTarget},
		{
			"Desktop options",
			"type=tmpfs,target=/scratch,consistency=cached,bind-create-src=true",
			"/scratch",
		},
		{
			"Docker zero defaults",
			"type=tmpfs,target=/scratch,tmpfs-mode=0000,tmpfs-size=0",
			"/scratch",
		},
		{
			"Docker size suffix",
			"type=tmpfs,target=/scratch,tmpfs-size=1mb",
			"/scratch:size=1048576",
		},
	}
}

func TestBuildRunArgsRejectsInvalidTmpfs(t *testing.T) {
	for _, spec := range []string{
		"type=tmpfs,source=bad,target=/scratch",
		"type=tmpfs,target=relative",
		"type=tmpfs,target=/scratch:other",
		"type=tmpfs,target=/scratch,tmpfs-mode",
		"type=tmpfs,target=/scratch,tmpfs-size=",
		"type=tmpfs,target=/scratch,unsupported=true",
		"type=tmpfs,target=/scratch,tmpfs-size=-1",
		"type=tmpfs,target=/scratch,tmpfs-size=oops",
		"type=tmpfs,target=/scratch,tmpfs-mode=0899",
		"type=tmpfs,target=/scratch,readonly=oops",
	} {
		t.Run(spec, func(t *testing.T) {
			mount := config.ParseMount(spec)
			params := &driver.RunImageDevContainerParams{
				ParsedConfig: &config.DevContainerConfig{},
				Options: &driver.RunOptions{
					Image:  testImageRef,
					Mounts: []*config.Mount{&mount},
				},
			}
			args, err := (&appleDriver{}).buildRunArgs(params)
			require.Error(t, err)
			require.Nil(t, args)
		})
	}
}

func TestBuildRunArgsPreservesRunArgs(t *testing.T) {
	raw := []string{
		"--tmpfs",
		"/native:mode=0700",
		"--mount",
		"type=tmpfs,target=/native-mount,mode=0755",
	}
	params := &driver.RunImageDevContainerParams{
		ParsedConfig: &config.DevContainerConfig{
			NonComposeBase: config.NonComposeBase{RunArgs: raw},
		},
		Options: &driver.RunOptions{Image: testImageRef},
	}
	args, err := (&appleDriver{}).buildRunArgs(params)
	require.NoError(t, err)
	require.Equal(
		t,
		append(append([]string{"run", "-d", "-l", "dev.containers.id="}, raw...), testImageRef),
		args,
	)
}
