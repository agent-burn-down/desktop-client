// Package normalize converts OTLP log records into NormalizedEvents using a
// strict metadata allowlist: prompt/completion/tool-payload free text is
// never copied into the output.
package normalize
