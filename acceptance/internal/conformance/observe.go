package conformance

import (
	"flag"
	"strings"
	"testing"
)

const markerKey, markerValue = "conformance", "unfiltered"

// Observe marks a full run for Report. Call it once in the root test.
// Focused runs behave normally but do not produce qualifying evidence.
func Observe(t *testing.T) {
	value := func(name string) string { return flag.Lookup("test." + name).Value.String() }
	if !testing.Short() && value("run") == "" && value("skip") == "" &&
		value("count") == "1" && !strings.Contains(value("cpu"), ",") {
		t.Attr(markerKey, markerValue)
	}
}
