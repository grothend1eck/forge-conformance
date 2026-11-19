//go:build acceptance

package forge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func runRequestTitle(t *testing.T, newFixture func(*testing.T) PublicationFixture) {
	t.Helper()
	f := newFixture(t)
	onlyRequest := func() PublicationRequest {
		t.Helper()
		requests := f.Driver.Requests(t)
		require.Len(t, requests, 1)
		require.True(t, requests[0].Open)
		return requests[0]
	}
	input := PublicationInput{Version: "v1.1.0"}
	f.Driver.Run(t, "apply", input)
	opened := onlyRequest()
	require.Equal(t, "Update the fixture version", opened.Title, "no override: use the pipeline title")
	refs := f.Repo.Refs(t)

	// A title in the manifest alone doesn't trigger an action. The next
	// target change should apply that very title to the same request.
	input.Title = `Release: "café" → stable`
	f.Driver.Run(t, "apply", input)
	require.Equal(t, opened, onlyRequest(), "a no-change run only reports the existing request")
	require.Equal(t, refs, f.Repo.Refs(t))

	input.Version = "v1.2.0"
	f.Driver.Run(t, "apply", input)
	edited := onlyRequest()
	require.Equal(t, opened.ID, edited.ID)
	require.Equal(t, opened.Number, edited.Number)
	require.Equal(t, input.Title, edited.Title)
}
