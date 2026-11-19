//go:build acceptance

package gitlab

import (
	"testing"

	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

func TestCapabilities(t *testing.T) {
	conformance.Observe(t)
	t.Run("gitlab", func(t *testing.T) {
		s := start(t)
		t.Run("publish_update", func(t *testing.T) { forge.RunPublishUpdate(t, s.newPublication) })
	})
}
