// Command conformance prints the capability catalogue, or checks a directory
// of Go test event streams laid out as <family>/<profile>.jsonl.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: conformance [results-directory]")
		os.Exit(2)
	}
	var err error
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
	files, err := filepath.Glob(filepath.Join(dir, family.Family, "*.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		streams[strings.TrimSuffix(filepath.Base(path), ".jsonl")] = f
	}
	return family.Report(os.Stdout, streams)
}
