package fixture

import (
	"os"
	"strings"
	"testing"

	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

func TestCapabilities(t *testing.T) {
	conformance.Observe(t)
	profile, mode, _ := strings.Cut(os.Getenv("CONFORMANCE_FIXTURE"), ":")
	if mode == "root:fail" {
		t.Fatal("private test output")
	}
	t.Run(profile, func(t *testing.T) {
		if mode == "partial" {
			return
		}
		t.Run("edit", func(t *testing.T) {
			t.Run("pick/path", func(t *testing.T) {
				t.Parallel()
				if mode == "skip" {
					t.Skip("private test output")
				}
				if mode == "fail" {
					t.Fatal("private test output")
				}
			})
		})
		if profile != "pending" {
			t.Run("read", func(*testing.T) {})
		}
		if group, ok := strings.CutPrefix(mode, "extra-"); ok {
			t.Run(group, func(*testing.T) {})
		}
	})
}
