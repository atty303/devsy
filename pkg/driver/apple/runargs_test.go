package apple

import (
	"strings"
	"testing"

	"github.com/devsy-org/devsy/pkg/devcontainer/config"
	"github.com/devsy-org/devsy/pkg/driver"
	"github.com/devsy-org/devsy/pkg/types"
	"github.com/stretchr/testify/require"
)

const testMountFlag = "--mount"

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

	args := d.buildRunArgs(params)
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

	off := (&appleDriver{}).buildRunArgs(params)
	if strings.Contains(strings.Join(off, " "), "--rosetta") {
		t.Errorf("--rosetta must not be present when Rosetta is disabled: %v", off)
	}

	on := (&appleDriver{Rosetta: true}).buildRunArgs(params)
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
	for _, tt := range []struct{ mount, want string }{
		{"type=tmpfs,target=/run/secrets,tmpfs-mode=0755", "type=tmpfs,dst=/run/secrets,mode=0755"},
		{"type=tmpfs,target=/run/devsy/secrets-env,tmpfs-mode=0700", "type=tmpfs,dst=/run/devsy/secrets-env,mode=0700"},
		{"type=tmpfs,target=/scratch,tmpfs-size=4096", "type=tmpfs,dst=/scratch,size=4096"},
		{
			"type=tmpfs,target=/scratch,tmpfs-mode=0700,future-option=enabled",
			"type=tmpfs,dst=/scratch,mode=0700,future-option=enabled",
		},
		{"type=tmpfs,target=/scratch,tmpfs-size=oops,tmpfs-mode=invalid", "type=tmpfs,dst=/scratch,size=oops,mode=invalid"},
		{
			"type=tmpfs,target=/scratch,tmpfs-mode=0777,tmpfs-mode=0700,readonly=false",
			"type=tmpfs,dst=/scratch,mode=0777,mode=0700,readonly=false",
		},
		{"type=tmpfs,target=/scratch,tmpfs-size=0,tmpfs-mode=0000", "type=tmpfs,dst=/scratch,size=0,mode=0000"},
	} {
		t.Run(tt.mount, func(t *testing.T) {
			mount := config.ParseMount(tt.mount)
			original := mount.String()
			params := &driver.RunImageDevContainerParams{
				ParsedConfig: &config.DevContainerConfig{},
				Options: &driver.RunOptions{
					Image:  testImageRef,
					Mounts: []*config.Mount{&mount},
				},
			}
			args := (&appleDriver{}).buildRunArgs(params)
			require.Equal(t, []string{testMountFlag, tt.want}, args[2:4])
			require.Equal(t, original, mount.String())
		})
	}
}

func TestBuildRunArgsPreservesOtherMountDirectives(t *testing.T) {
	for _, spec := range []string{
		"type=tmpfs,source=bad,target=/scratch", "type=tmpfs,target=relative", "type=tmpfs,target=/scratch:other",
		"type=tmpfs,target=/scratch,tmpfs-mode", "type=tmpfs,target=/scratch,mode=0700,size=1M",
		"type=tmpfs,target=/scratch,future-option=enabled", "type=tmpfs,target=/scratch,consistency",
		"type=tmpfs,target=/scratch,Consistency=cached", "type=tmpfs,target=/scratch,bind-create-src",
		"type=volume,source=volume,target=/scratch,tmpfs-mode=0700",
	} {
		t.Run(spec, func(t *testing.T) {
			mount := config.ParseMount(spec)
			require.Equal(t, []string{testMountFlag, mount.String()}, appendMount(nil, &mount))
		})
	}
}

func TestBuildRunArgsPreservesRunArgs(t *testing.T) {
	raw := []string{
		"--tmpfs",
		"/native:mode=0700",
		testMountFlag,
		"type=tmpfs,target=/native-mount,mode=0755",
	}
	params := &driver.RunImageDevContainerParams{
		ParsedConfig: &config.DevContainerConfig{
			NonComposeBase: config.NonComposeBase{RunArgs: raw},
		},
		Options: &driver.RunOptions{Image: testImageRef},
	}
	args := (&appleDriver{}).buildRunArgs(params)
	require.Equal(
		t,
		append(append([]string{"run", "-d", "-l", "dev.containers.id="}, raw...), testImageRef),
		args,
	)
}

func TestAppendMountKeepsLegacyCleaningOnFallback(t *testing.T) {
	mount := config.ParseMount(
		"type=tmpfs,target=/scratch,mode=0700,consistency=cached,bind-create-src=true",
	)
	require.Equal(
		t,
		[]string{testMountFlag, "type=tmpfs,dst=/scratch,mode=0700"},
		appendMount(nil, &mount),
	)
}
