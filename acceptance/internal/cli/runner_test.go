//go:build acceptance

package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func TestLocalYAMLPipeline(t *testing.T) {
	const (
		current   = "v1.2.3"
		candidate = "v4.5.6"
		keep      = "\nchannel: stable\n"
		before    = "version: " + current + keep
		after     = "version: " + candidate + keep
		manifest  = `name: Update a local YAML file
targets:
  version:
    kind: yaml
    spec:
      file: version.yaml
      key: $.version
      value: '{{ requiredEnv "UPDATECLI_ACCEPTANCE_VERSION" }}'
`
	)

	runner := cli.New(t, "UPDATECLI_ACCEPTANCE_VERSION="+candidate)
	file := filepath.Join(runner.Dir, "version.yaml")
	require.NoError(t, os.WriteFile(file, []byte(before), 0o600))
	preview := runner.PipelineOK(t, "diff", manifest)
	cli.RequireOutput(t, preview, current, candidate)
	checkFile(t, file, before)
	runner.PipelineOK(t, "apply", manifest)
	checkFile(t, file, after)

	// A value present only in the parent must not satisfy requiredEnv.
	t.Setenv("UPDATECLI_ACCEPTANCE_VERSION", candidate)
	isolated := cli.New(t)
	missing := isolated.Run(t, "pipeline", "diff", "--config", filepath.Join(runner.Dir, "updatecli.yaml"))
	require.NotZero(t, missing.ExitCode)
	require.Contains(t, missing.Output, "UPDATECLI_ACCEPTANCE_VERSION")
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func TestRunWithFixturePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture executable requires /bin/sh")
	}

	const manifest = `name: Use a fixture tool on PATH
targets:
  fixture:
    kind: shell
    disablesourceinput: true
    spec:
      command: |
        {{ if or (env "UPDATECLI_ACCEPTANCE_AMBIENT_SECRET")
          (eq (env "TMPDIR") "fixture-override") (eq (env "HTTP_PROXY") "fixture-override") }}
        echo 'runner environment is not isolated' >&2
        exit 1
        {{ end }}
        updatecli-acceptance-fixture-tool
`
	t.Setenv("UPDATECLI_ACCEPTANCE_AMBIENT_SECRET", "fake-secret")
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "updatecli-acceptance-fixture-tool"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$PATH\" \"$HOME\" > fixture-environment\n"), 0o700))

	hostPath := tools + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", hostPath)
	for _, tt := range []struct {
		name     string
		env      []string
		wantPath string
	}{
		{name: "default", wantPath: hostPath},
		{
			name:     "override",
			env:      []string{"PATH=" + tools, "HOME=fixture-override", "TMPDIR=fixture-override", "HTTP_PROXY=fixture-override"},
			wantPath: tools,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := cli.New(t, tt.env...)
			runner.PipelineOK(t, "apply", manifest)
			checkFile(t, filepath.Join(runner.Dir, "fixture-environment"), tt.wantPath+"\n"+runner.Home+"\n")
			require.Equal(t, hostPath, os.Getenv("PATH"))
		})
	}
}

func TestAnonymousExternalHostOptIn(t *testing.T) {
	t.Setenv("UPDATECLI_GITEA_USERNAME", "ambient-user")
	t.Setenv("UPDATECLI_GITEA_TOKEN", "ambient-token")
	t.Setenv("NO_PROXY", "*")
	const manifest = `name: Inspect candidate environment
targets:
  no_proxy:
    kind: file
    disablesourceinput: true
    spec:
      file: no-proxy
      content: '{{ env "NO_PROXY" }}|{{ env "no_proxy" }}|{{ env "UPDATECLI_GITEA_USERNAME" }}|{{ env "UPDATECLI_GITEA_TOKEN" }}'
`
	const loopback = "127.0.0.1,localhost,::1"
	for _, tc := range []struct {
		runner *cli.Runner
		want   string
	}{
		{cli.New(t), loopback},
		{cli.NewAnonymousExternal(t, "example.test"), loopback + ",example.test"},
	} {
		path := filepath.Join(tc.runner.Dir, "no-proxy")
		require.NoError(t, os.WriteFile(path, []byte("unset"), 0o600))
		tc.runner.PipelineOK(t, "apply", manifest)
		checkFile(t, path, tc.want+"|"+tc.want+"||")
	}
}
