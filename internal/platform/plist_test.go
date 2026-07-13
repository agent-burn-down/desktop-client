//go:build darwin

package platform

import (
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

func TestRenderPlistGolden(t *testing.T) {
	got := renderPlist(plistParams{
		Label:      Label,
		Program:    "/usr/local/bin/burndown-cli",
		Args:       []string{"serve"},
		StdoutPath: "/Users/test/.burndown/logs/collector.out.log",
		StderrPath: "/Users/test/.burndown/logs/collector.err.log",
	})
	golden := filepath.Join("testdata", "collector.plist")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("plist mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
	// The rendered plist must be well-formed XML.
	var v any
	if err := xml.Unmarshal(got, &v); err != nil {
		t.Errorf("rendered plist is not valid XML: %v", err)
	}
}

func TestRenderPlistEscapesPaths(t *testing.T) {
	got := renderPlist(plistParams{
		Label:      Label,
		Program:    "/opt/A & B/burndown-cli",
		Args:       []string{"serve"},
		StdoutPath: "/tmp/out.log",
		StderrPath: "/tmp/err.log",
	})
	if got == nil {
		t.Fatal("nil plist")
	}
	var v any
	if err := xml.Unmarshal(got, &v); err != nil {
		t.Fatalf("plist with special chars is not valid XML: %v", err)
	}
	if want := "/opt/A &amp; B/burndown-cli"; !strings.Contains(string(got), want) {
		t.Errorf("ampersand not escaped; want %q in output", want)
	}
}
