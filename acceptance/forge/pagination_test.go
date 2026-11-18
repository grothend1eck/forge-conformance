//go:build acceptance

package forge

import (
	"fmt"
	"testing"

	"gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

func seedReleaseOnLaterPage(t *testing.T, s *server) (*repository, *gitea.Release) {
	t.Helper()
	r := s.newRepo(t)
	const pageSize = 30
	seeded := make(map[string]int64, pageSize+1)
	for i := 1; i <= pageSize+1; i++ {
		tag := fmt.Sprintf("v1.0.%d", i)
		release, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
			TagName: tag, Target: r.base, Title: tag,
		})
		if err != nil {
			s.apiFailure(t, "seed paginated release "+tag, resp, err)
		}
		require.Positive(t, release.ID)
		seeded[tag] = release.ID
	}

	listPage := func(page int) []*gitea.Release {
		t.Helper()
		releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, r.name, gitea.ListReleasesOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
		})
		if err != nil {
			s.apiFailure(t, fmt.Sprintf("read releases page %d", page), resp, err)
		}
		return releases
	}
	first, second := listPage(1), listPage(2)
	require.Len(t, first, pageSize)
	require.Len(t, second, 1)
	later := second[0]
	require.False(t, later.IsDraft)
	require.Contains(t, seeded, later.TagName)
	require.Equal(t, seeded[later.TagName], later.ID)
	for _, release := range first {
		require.NotEqual(t, later.TagName, release.TagName, "matched tag must be absent from page 1")
		require.NotEqual(t, later.ID, release.ID, "matched release ID must be absent from page 1")
	}
	t.Logf("release %s (ID %d) is on page 2 and absent from page 1", later.TagName, later.ID)
	return r, later
}
