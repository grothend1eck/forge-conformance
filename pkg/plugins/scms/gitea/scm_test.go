package gitea

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewWithSchemelessURL(t *testing.T) {
	scm, err := New(map[string]any{
		"url":        "forge.example.test",
		"owner":      "example",
		"repository": "fixture",
		"directory":  t.TempDir(),
	}, "fixture")
	require.NoError(t, err)
	require.Equal(t, "https://forge.example.test/example/fixture.git", scm.GetURL())
}
