//go:build acceptance && candidate

package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/require"

	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func TestCandidateStampAndManifestVersionGuard(t *testing.T) {
	runner := cli.New(t)
	result := runner.Run(t, "version", "--disable-version-check")
	require.Zero(t, result.ExitCode, "version output:\n%s", result.Output)
	t.Log(result.Output)

	application := versionField(t, result.Output, "Application:")
	if expected := os.Getenv("UPDATECLI_EXPECTED_VERSION"); expected != "" {
		require.Equal(t, expected, application)
	}
	tag, ok := strings.CutSuffix(application, "-next")
	require.True(t, ok, "expected a GoReleaser snapshot version, got %q", application)
	rawVersion, ok := strings.CutPrefix(tag, "v")
	require.True(t, ok, "expected a release tag in snapshot version %q", application)
	base, err := semver.StrictNewVersion(rawVersion)
	require.NoError(t, err, "invalid snapshot base tag %q", tag)
	releaseLine := fmt.Sprintf("%d.%d.%d", base.Major(), base.Minor(), base.Patch())
	require.NotEqual(t, "0.0.0", releaseLine, "snapshot must have a real release base")
	require.Regexp(t, `^\S+ \S+/\S+$`, versionField(t, result.Output, "Golang     :"))
	buildTime, err := time.Parse(time.RFC3339Nano, versionField(t, result.Output, "Build Time :"))
	require.NoError(t, err, "invalid snapshot build time")
	require.False(t, buildTime.IsZero())

	const (
		before   = "value: original\n"
		after    = "value: changed\n"
		manifest = `name: Minimum CLI version
version: %q
targets:
  update:
    kind: yaml
    spec:
      file: state.yaml
      key: $.value
      value: changed
`
	)
	file := filepath.Join(runner.Dir, "state.yaml")
	require.NoError(t, os.WriteFile(file, []byte(before), 0o600))

	denied := runner.Pipeline(t, "apply", fmt.Sprintf(manifest, "999.0.0"))
	require.NotZero(t, denied.ExitCode, "incompatible apply output:\n%s", denied.Output)
	require.Contains(t, denied.Output, `pipeline "Minimum CLI version" requires Updatecli version greater than or equal to "999.0.0", skipping`)
	unchanged, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, string(unchanged))

	// The same target is writable when the only change is a compatible minimum.
	allowed := runner.Pipeline(t, "apply", fmt.Sprintf(manifest, releaseLine))
	require.Zero(t, allowed.ExitCode, "compatible apply output:\n%s", allowed.Output)
	changed, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, after, string(changed))
}

func versionField(t *testing.T, output, label string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), label); found {
			value = strings.TrimSpace(value)
			require.NotEmpty(t, value, "empty %s in version output:\n%s", label, output)
			return value
		}
	}
	t.Fatalf("missing %s in version output:\n%s", label, output)
	return ""
}
