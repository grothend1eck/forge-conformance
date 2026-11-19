// Command fuzzmatrix replays Go's seed corpora and prints the targets for CI.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type target struct {
	Package string `json:"package"`
	Name    string `json:"name"`
}

func main() {
	log.SetFlags(0)
	targets, err := discover()
	if err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(targets); err != nil {
		log.Fatal(err)
	}
}

func discover() ([]target, error) {
	// go list respects build tags, nested modules and external test packages.
	// Only compile packages that could contain a fuzz test. A comment may
	// select an extra package; only Go's actual test events create a target.
	list := exec.Command("go", "list", "-find", "-json=Dir,ImportPath,TestGoFiles,XTestGoFiles", "./...")
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	args := []string{"test", "-json", "-count=1", "-run=^Fuzz"}
	d := json.NewDecoder(bytes.NewReader(out))
	for d.More() {
		var p struct {
			Dir, ImportPath           string
			TestGoFiles, XTestGoFiles []string
		}
		if err := d.Decode(&p); err != nil {
			return nil, err
		}
		for _, f := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
			src, err := os.ReadFile(filepath.Join(p.Dir, f))
			if err != nil {
				return nil, err
			}
			if bytes.Contains(src, []byte("Fuzz")) {
				args = append(args, p.ImportPath)
				dirs[p.ImportPath] = p.Dir
				break
			}
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no Go fuzz targets found")
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()
	var targets []target
	d = json.NewDecoder(bytes.NewReader(out))
	for d.More() {
		var e struct{ Action, Test, Package, Output string }
		if err := d.Decode(&e); err != nil {
			return nil, err
		}
		if runErr != nil {
			fmt.Fprint(os.Stderr, e.Output)
			continue
		}
		if !strings.HasPrefix(e.Test, "Fuzz") || strings.Contains(e.Test, "/") {
			continue
		}
		switch e.Action {
		case "skip":
			return nil, fmt.Errorf("%s: %s skipped", e.Package, e.Test)
		case "pass":
			dir, err := filepath.Rel(wd, dirs[e.Package])
			if err != nil {
				return nil, err
			}
			targets = append(targets, target{filepath.ToSlash(dir), e.Test})
		}
	}
	if runErr != nil {
		return nil, runErr
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no Go fuzz targets found")
	}
	slices.SortFunc(targets, func(a, b target) int {
		if a.Package != b.Package {
			return strings.Compare(a.Package, b.Package)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return targets, nil
}
