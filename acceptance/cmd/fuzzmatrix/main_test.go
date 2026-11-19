package main

import (
	"os"
	"slices"
	"testing"
)

func TestDiscoversEachFuzzTarget(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GOWORK", "off")
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/checks\n\ngo 1.27\n")
	if err := os.Mkdir("other", 0o700); err != nil {
		t.Fatal(err)
	}
	const external = `package checks_test
import "testing"
func FuzzEdit(f *testing.F) { f.Add("a"); f.Fuzz(func(*testing.T, string) {}) }
`
	write("one_test.go", external+`func FuzzEditAgain(f *testing.F) { FuzzEdit(f) }`)
	write("other/two_test.go", external)
	write("disabled_test.go", "//go:build neverbuilt\n\n"+external)
	want := []target{{".", "FuzzEdit"}, {".", "FuzzEditAgain"}, {"other", "FuzzEdit"}}
	got, err := discover()
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v: %v", got, want, err)
	}
	write("other/two_test.go", `package checks_test
import "testing"
func FuzzEdit(f *testing.F) { f.Fatal("failing seed setup") }
`)
	if _, err := discover(); err == nil {
		t.Fatal("failing fuzz target qualified")
	}
}
