//go:build acceptance

package cli_test

import (
	"os"
	"path/filepath"
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
	checkFile := func(want string) {
		t.Helper()
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}

	preview := runner.Pipeline(t, "diff", manifest)
	require.Zero(t, preview.ExitCode, "diff output:\n%s", preview.Output)
	require.Contains(t, preview.Output, current)
	require.Contains(t, preview.Output, candidate)
	checkFile(before)

	applied := runner.Pipeline(t, "apply", manifest)
	require.Zero(t, applied.ExitCode, "apply output:\n%s", applied.Output)
	checkFile(after)

	// A value present only in the parent must not satisfy requiredEnv.
	t.Setenv("UPDATECLI_ACCEPTANCE_VERSION", candidate)
	isolated := cli.New(t)
	missing := isolated.Run(t, "pipeline", "diff", "--config", filepath.Join(runner.Dir, "updatecli.yaml"))
	require.NotZero(t, missing.ExitCode)
	require.Contains(t, missing.Output, "UPDATECLI_ACCEPTANCE_VERSION")
}
