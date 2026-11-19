package release

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/pkg/core/result"
	"github.com/updatecli/updatecli/pkg/plugins/utils/pathresolver"
)

type releaseLookups func(*http.Request) (*http.Response, error)

func (f releaseLookups) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTargetStopsOnPaginatedLookupError(t *testing.T) {
	transportErr := errors.New("release API unavailable")
	for _, tc := range []struct {
		name         string
		transportErr error
	}{
		{name: "unexpected JSON object"},
		{name: "no HTTP response", transportErr: transportErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := New(map[string]string{
				"url": "https://fixture.invalid", "owner": "fixture", "repository": "repo",
				"tag": "v1.0.0", "commitish": "main", "token": "fixture-token",
			})
			require.NoError(t, err)

			var pages []string
			g.client.Client = &http.Client{Transport: releaseLookups(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodGet, req.Method, "failed lookups must not attempt release creation")
				require.Equal(t, "/api/v1/repos/fixture/repo/releases", req.URL.Path)
				page := req.URL.Query().Get("page")
				pages = append(pages, page)
				switch page {
				case "1":
					return &http.Response{
						StatusCode: http.StatusOK,
						Header: http.Header{
							"Link": {`<https://fixture.invalid/api/v1/repos/fixture/repo/releases?page=7>; rel="next"`},
						},
						Body: io.NopCloser(strings.NewReader(`[{"tag_name":"v0.9.0"}]`)),
					}, nil
				case "7":
					if tc.transportErr != nil {
						return nil, tc.transportErr
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"message":"not a release list"}`)),
					}, nil
				default:
					t.Fatalf("unexpected page %q", page)
					return nil, nil
				}
			})}

			var got result.Target
			err = g.Target(t.Context(), "", nil, pathresolver.Resolver{}, false, &got)
			require.ErrorContains(t, err, "list Gitea releases on page 7")
			if tc.transportErr != nil {
				require.ErrorIs(t, err, tc.transportErr)
			} else {
				require.ErrorAs(t, err, new(*json.UnmarshalTypeError))
			}
			require.False(t, got.Changed)
			require.Equal(t, []string{"1", "7"}, pages)
		})
	}
}
