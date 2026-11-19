//go:build acceptance && live

package live_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
	"go.yaml.in/yaml/v4"
)

// Decode the candidate's external report format without sharing its result logic.
type codebergReport struct {
	Name, Result, Err            string
	Actions                      map[string]any
	Sources, Conditions, Targets map[string]codebergResource
}

type codebergResource struct {
	Result, Information, NewInformation, Description, SourceID string
	Pass, Changed                                              bool
	Files                                                      []string
	SCM                                                        struct {
		URL         string
		Branch      struct{ Source, Target string }
		BranchReset bool
	}
}

// TestCodeberg preserves the public, anonymous Venom pipeline check separately
// from local forge conformance. It never applies or publishes changes.
func TestCodeberg(t *testing.T) {
	config, err := filepath.Abs("../testdata/codeberg")
	require.NoError(t, err)

	preview := cli.NewAnonymousExternal(t, "codeberg.org").Run(t, "pipeline", "diff", "--config", config,
		"--disable-version-check", "--disable-udash-report", "--export-report-to-yaml")
	require.Zero(t, preview.ExitCode, "%s", preview.Output)
	require.NotContains(t, preview.Output, "WARNING:")
	require.NotContains(t, preview.Output, "ERROR:")
	// The pipeline banner reflects diff mode; the per-target report's DryRun
	// field does not reflect the pipeline option.
	require.Equal(t, 4, strings.Count(preview.Output, "\nDry Run\t\t: enabled\n"))
	paths, err := filepath.Glob(filepath.Join(preview.TempDir, "updatecli", "report", "*", "*.yaml"))
	require.NoError(t, err)
	require.Len(t, paths, 4)
	reports := make(map[string]codebergReport)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var report codebergReport
		require.NoError(t, yaml.Unmarshal(data, &report))
		require.NotContains(t, reports, report.Name)
		reports[report.Name] = report
	}

	for _, tc := range []struct {
		manifest                     string
		sources, conditions, targets []string
	}{
		{"branch", []string{"default"}, []string{"default", "sourcedefault"}, nil},
		{"release", []string{"default"}, []string{"default", "sourcedefault"}, nil},
		{"scm", []string{"license", "readme"}, []string{"license", "readme"}, []string{"license", "readme"}},
		{"tag", []string{"default", "latest", "mirror"}, []string{"default", "sourcedefault", "latest"}, nil},
	} {
		t.Run(tc.manifest, func(t *testing.T) {
			report, ok := reports["Test Gitea "+tc.manifest]
			require.True(t, ok, "missing pipeline report")
			// The legacy receipts had no skipped pipelines or resources, including
			// the inverted tag condition and the two unchanged targets.
			require.Equal(t, "✔", report.Result)
			require.Empty(t, report.Err)
			require.Empty(t, report.Actions)
			requireCodebergResources(t, "source", report.Sources, tc.sources)
			requireCodebergResources(t, "condition", report.Conditions, tc.conditions)
			requireCodebergResources(t, "target", report.Targets, tc.targets)
			for id, source := range report.Sources {
				require.NotEmpty(t, source.Information, "source %s did not return a value", id)
			}
			for id, condition := range report.Conditions {
				require.True(t, condition.Pass, "condition %s did not pass", id)
			}

			switch tc.manifest {
			case "branch", "release", "tag":
				selected := report.Sources["default"].Information
				if tc.manifest != "branch" {
					version, err := semver.NewVersion(selected)
					require.NoError(t, err)
					filter, err := semver.NewConstraint("~2")
					require.NoError(t, err)
					require.True(t, filter.Check(version), "expected %s to match ~2", selected)
					t.Logf("semver ~2 selected %s", selected)
				}
				requireCodebergRef(t, report, "sourcedefault", "default", tc.manifest, selected)
				const explicitBranch, explicitRelease, absentTag = "main", "v2.15.0", "v2.15.2"
				if tc.manifest == "tag" {
					require.Equal(t, fmt.Sprintf("no Gitea tag found matching %q", absentTag), report.Conditions["default"].Description)
					latest := report.Sources["latest"].Information
					require.Equal(t, latest, report.Sources["mirror"].Information)
					requireCodebergRef(t, report, "latest", "latest", "tag", latest)
					t.Logf("latest and mirror selected %s; inverted condition confirmed %s absent", latest, absentTag)
				} else {
					ref := explicitBranch
					if tc.manifest == "release" {
						ref = explicitRelease
					}
					require.Contains(t, report.Conditions["default"].Description, fmt.Sprintf("%s %q found", tc.manifest, ref))
					t.Logf("selected %s and confirmed explicit %s", selected, ref)
				}
			case "scm":
				for id, file := range map[string]string{"license": "LICENSE", "readme": "README.md"} {
					source, condition, target := report.Sources[id], report.Conditions[id], report.Targets[id]
					require.Contains(t, source.Description, fmt.Sprintf("found from file %q", file))
					require.Contains(t, condition.Description, fmt.Sprintf("[%q] passed", file))
					require.Equal(t, id, condition.SourceID)
					require.Equal(t, id, target.SourceID)
					require.True(t, source.Information == target.NewInformation, "%s target did not use the source content", file)
					require.False(t, target.Changed)
					require.Empty(t, target.Files)
					require.Contains(t, target.Description, "already up to date")
					require.False(t, target.SCM.BranchReset)
					for _, resource := range []codebergResource{source, condition, target} {
						require.Equal(t, "https://codeberg.org/updatecli/demo-terminal.git", resource.SCM.URL)
						require.Equal(t, "main", resource.SCM.Branch.Source)
						require.Equal(t, "main", resource.SCM.Branch.Target)
					}
					t.Logf("%s: public SCM read, matching condition and unchanged dry-run target", file)
				}
			}
		})
	}
}

func requireCodebergResources(t *testing.T, category string, resources map[string]codebergResource, ids []string) {
	t.Helper()
	require.Len(t, resources, len(ids), "%s count", category)
	for _, id := range ids {
		require.Equal(t, "✔", resources[id].Result, "%s %s must have run successfully", category, id)
	}
}

func requireCodebergRef(t *testing.T, report codebergReport, condition, source, kind, value string) {
	t.Helper()
	got := report.Conditions[condition]
	require.Equal(t, source, got.SourceID)
	require.Contains(t, got.Description, fmt.Sprintf("%s %q found", kind, value))
}
