//go:build acceptance

package forge

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func manifestYAML(t *testing.T, spec map[string]any) string {
	t.Helper()
	b, err := yaml.Marshal(spec)
	require.NoError(t, err)
	return string(b)
}

// The providers supply their real SCM and action; the file edit is the same.
func publicationConfig(id, version string, scm, action map[string]any) map[string]any {
	return map[string]any{
		"name":       "Update the fixture version",
		"pipelineid": id,
		"scms":       map[string]any{"origin": scm},
		"targets": map[string]any{
			"version": map[string]any{
				"name": "Update the version", "kind": "yaml", "scmid": "origin",
				"spec": map[string]any{"file": "version.yaml", "key": "$.version", "value": version},
			},
		},
		"actions": map[string]any{"request": action},
	}
}

// Leave the Gitea action's connection and branches for the SCM to supply.
func requestManifest(t *testing.T, r *repository, id, version string, spec map[string]any) string {
	t.Helper()
	return manifestYAML(t, requestConfig(r, id, version, spec))
}

func requestConfig(r *repository, id, version string, spec map[string]any) map[string]any {
	scm := map[string]any{"kind": "gitea", "spec": map[string]any{
		"url": r.server.url, "owner": r.server.owner, "repository": r.name, "branch": r.base,
	}}
	action := map[string]any{"kind": "gitea/pullrequest", "scmid": "origin"}
	if len(spec) > 0 {
		action["spec"] = spec
	}
	return publicationConfig(id, version, scm, action)
}
