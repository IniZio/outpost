package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var settingName = regexp.MustCompile(`OUTPOST_[A-Z_]+`)

// boxOnly are settings Outpost sets inside a box, which no operator sets.
var boxOnly = []string{"OUTPOST_MTU"}

func namesIn(t *testing.T, dirs ...string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				addNames(t, found, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

func addNames(t *testing.T, found map[string]bool, file string) {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // the repo's own files
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range settingName.FindAllString(string(b), -1) {
		found[n] = true
	}
}

// absent lists the names in a that b lacks, leaving out skip.
func absent(a, b map[string]bool, skip []string) []string {
	var out []string
	for n := range a {
		if !b[n] && !slices.Contains(skip, n) {
			out = append(out, n)
		}
	}
	return out
}

func TestTheEnvTemplateListsEverySetting(t *testing.T) {
	code := namesIn(t, "../../cmd", "../../internal")
	tpl, err := os.ReadFile("../../deploy/env.example")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, n := range settingName.FindAllString(string(tpl), -1) {
		listed[n] = true
	}
	if missing := absent(code, listed, boxOnly); len(missing) > 0 {
		t.Errorf("read by the code but missing from deploy/env.example: %v", missing)
	}
	if unused := absent(listed, code, nil); len(unused) > 0 {
		t.Errorf("listed in deploy/env.example but read by nothing: %v", unused)
	}
}
