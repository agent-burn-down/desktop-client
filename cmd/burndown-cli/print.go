package main

import (
	"fmt"
	"io"
)

// outf and outln write user-facing output, discarding the write error: a failed
// write to stdout or stderr is neither recoverable nor actionable here.
func outf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func outln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}
