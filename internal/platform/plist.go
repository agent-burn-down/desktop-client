//go:build darwin

package platform

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// plistParams are the injected values a launchd plist is rendered from. Keeping
// every path a parameter makes the renderer a pure function with a golden test.
type plistParams struct {
	Label      string
	Program    string
	Args       []string
	StdoutPath string
	StderrPath string
}

// renderPlist produces the launchd property list for the collector service.
// RunAtLoad starts it on load/login; KeepAlive restarts it if it crashes.
func renderPlist(p plistParams) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writeStringEntry(&b, "Label", p.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range append([]string{p.Program}, p.Args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", escape(arg))
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	writeStringEntry(&b, "StandardOutPath", p.StdoutPath)
	writeStringEntry(&b, "StandardErrorPath", p.StderrPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func writeStringEntry(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<string>%s</string>\n", key, escape(value))
}

// escape XML-encodes a value so paths containing &, <, or quotes stay valid.
func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
