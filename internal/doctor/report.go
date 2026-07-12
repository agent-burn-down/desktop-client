package doctor

import (
	"encoding/json"
	"fmt"
	"io"
)

// resultJSON is the machine-readable shape of a single check.
type resultJSON struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// reportJSON is the top-level --json document: the aggregate status plus every
// check result.
type reportJSON struct {
	Status  string       `json:"status"`
	Results []resultJSON `json:"results"`
}

// WriteJSON emits the results as an indented JSON document with a stable shape.
func WriteJSON(w io.Writer, results []Result) error {
	doc := reportJSON{
		Status:  Aggregate(results).String(),
		Results: make([]resultJSON, len(results)),
	}
	for i, r := range results {
		doc.Results[i] = resultJSON{
			Name:   r.Name,
			Status: r.Status.String(),
			Detail: r.Detail,
			Hint:   r.Hint,
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// WriteText emits a plain-text table: one line per check with its status,
// detail, and (for warn/fail) a remediation hint on the following line. A write
// error to stdout is neither recoverable nor actionable, so it is discarded.
func WriteText(w io.Writer, results []Result) {
	for _, r := range results {
		_, _ = fmt.Fprintf(w, "[%-4s] %-10s %s\n", r.Status.String(), r.Name, r.Detail)
		if r.Hint != "" {
			_, _ = fmt.Fprintf(w, "         fix: %s\n", r.Hint)
		}
	}
	_, _ = fmt.Fprintf(w, "\noverall: %s\n", Aggregate(results).String())
}
