package platform

import (
	"errors"
	"runtime"
	"testing"
)

func TestOpenURLUsesGOOSAppropriateCommand(t *testing.T) {
	var gotName string
	var gotArgs []string
	orig := commander
	defer func() { commander = orig }()
	commander = func(name string, args ...string) error {
		gotName, gotArgs = name, args
		return nil
	}

	if err := OpenURL("https://app.example/activate?code=ABCD-1234"); err != nil {
		t.Fatalf("OpenURL: %v", err)
	}

	want := map[string]string{"darwin": "open", "windows": "rundll32"}[runtime.GOOS]
	if want == "" {
		want = "xdg-open"
	}
	if gotName != want {
		t.Errorf("command = %q, want %q for GOOS=%s", gotName, want, runtime.GOOS)
	}
	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != "https://app.example/activate?code=ABCD-1234" {
		t.Errorf("args = %v, want the URL as the final argument", gotArgs)
	}
}

func TestOpenURLNeverFatalOnCommandFailure(t *testing.T) {
	orig := commander
	defer func() { commander = orig }()
	commander = func(string, ...string) error {
		return errors.New("no such binary")
	}

	// OpenURL surfaces the error to the caller, but does not panic or block;
	// callers are expected to log-and-continue rather than treat it as fatal.
	if err := OpenURL("https://app.example/activate"); err == nil {
		t.Fatal("expected the stubbed command failure to propagate as an error")
	}
}
