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

func TestSnapshotVersionGuard(t *testing.T) {
	r := cli.New(t)
	version := r.Run(t, "version", "--disable-version-check")
	require.Zero(t, version.ExitCode)
	fields := map[string]string{}
	for line := range strings.SplitSeq(version.Output, "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	app := fields["Application"]
	if expected := os.Getenv("UPDATECLI_EXPECTED_VERSION"); expected != "" {
		require.Equal(t, expected, app)
	}
	require.Regexp(t, `^v.+-next$`, app)
	tag, err := semver.StrictNewVersion(strings.TrimSuffix(strings.TrimPrefix(app, "v"), "-next"))
	require.NoError(t, err)
	minimum := fmt.Sprintf("%d.%d.%d", tag.Major(), tag.Minor(), tag.Patch())
	require.NotEqual(t, "0.0.0", minimum)
	require.Regexp(t, `^\S+ \S+/\S+$`, fields["Golang"])
	built, err := time.Parse(time.RFC3339Nano, fields["Build Time"])
	require.NoError(t, err)
	require.False(t, built.IsZero())

	const manifest = `name: Minimum CLI version
version: %q
targets:
  edit:
    kind: yaml
    spec: {file: state.yaml, key: $.value, value: changed}
`
	file := filepath.Join(r.Dir, "state.yaml")
	require.NoError(t, os.WriteFile(file, []byte("value: original\n"), 0o600))
	denied := r.Pipeline(t, "apply", fmt.Sprintf(manifest, "999.0.0"))
	require.NotZero(t, denied.ExitCode)
	require.Contains(t, denied.Output, `pipeline "Minimum CLI version" requires Updatecli version greater than or equal to "999.0.0", skipping`)
	checkFile(t, file, "value: original\n")
	r.PipelineOK(t, "apply", fmt.Sprintf(manifest, minimum))
	checkFile(t, file, "value: changed\n")
}
