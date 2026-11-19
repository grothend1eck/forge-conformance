// Command conformance prints the capability catalogue, or checks a directory
// of Go test event streams laid out as <family>/<profile>.jsonl.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: conformance [results-directory]")
		os.Exit(2)
	}
	var err error
	// Each family owns its matrix. The renderer has no provider-specific code.
	for _, family := range []conformance.Matrix{forge.Matrix} {
		if len(os.Args) == 1 {
			err = errors.Join(err, family.Overview(os.Stdout))
		} else {
			err = errors.Join(err, report(family, os.Args[1]))
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func report(family conformance.Matrix, dir string) error {
	streams := make(map[string]io.Reader)
	for _, id := range family.Profiles {
		f, err := os.Open(filepath.Join(dir, family.Family, id+".jsonl"))
		if errors.Is(err, os.ErrNotExist) {
			continue // Report marks the missing profile unverified in the summary.
		}
		if err != nil {
			return err
		}
		defer f.Close()
		streams[id] = f
	}
	return family.Report(os.Stdout, streams)
}
