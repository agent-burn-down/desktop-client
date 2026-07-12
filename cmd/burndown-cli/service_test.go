package main

import "testing"

func TestServiceSubcommandsWired(t *testing.T) {
	cmd := newServiceCmd()
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[c.Name()] = true
	}
	for _, want := range []string{"install", "uninstall", "start", "stop", "status"} {
		if !got[want] {
			t.Errorf("service command missing subcommand %q", want)
		}
	}
}
