// Package credstore protects the collector's active and pending API keys at
// rest, moving them out of internal/config's plaintext JSON file and into an
// OS keychain where one is available (darwin only today), with the file as
// an explicit, reported fallback.
package credstore
