package codexrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

// fallbackHashHexLen is the length of the hex-encoded tier-3 key body: the
// first 12 hex characters (48 bits) of the salted path hash.
const fallbackHashHexLen = 12

// scpLikeRemote matches git's scp-like remote syntax, [user@]host:path,
// distinguishing it from a URL (which contains "://") and from an absolute
// or Windows-style local path (which starts with "/" or "<letter>:\").  The
// host group excludes "/" so a path never matches, and matchedHost rejects a
// single-letter host so a Windows drive ("C:\...") isn't mistaken for one.
var scpLikeRemote = regexp.MustCompile(`^(?:[^@/\s]+@)?([^/\s:]+):(.+)$`)

// RepoKey returns a privacy-safe, stable identity for cwd suitable for
// correlating one repository's telemetry across clones and machines without
// ever transmitting a filesystem path. Three tiers, first match wins:
//
//  1. named:<slug> -- a user-declared override. No config surface exists for
//     this yet (lands in #78); namedRepoKey always falls through today.
//  2. <host>/<owner>/<name> -- the git origin remote, normalized.
//  3. local:<12 hex> -- a salted hash of the resolved repo path, when no
//     usable remote exists.
//
// RepoKey returns "" exactly when canonicalRepo(cwd) does: an unresolvable
// cwd stays unattributed. A directory that resolves but isn't a git
// repository -- including a workspace root containing git repos as children
// -- still gets a tier-3 key, distinct from any child repo's key.
//
// Like canonicalRepo, resolution never fails: a missing git binary, a
// timeout, or a malformed remote falls through to the next tier rather than
// erroring, so a caller can always attach RepoKey's result to an event.
func RepoKey(cwd string) string {
	if canonicalRepo(cwd) == "" {
		return ""
	}
	path := resolvedRepoPath(cwd)
	if named := namedRepoKey(path); named != "" {
		return "named:" + named
	}
	if remote := gitOriginRemote(path); remote != "" {
		if key := normalizeRemote(remote); key != "" {
			return key
		}
	}
	return "local:" + fallbackHash(path)
}

// namedRepoKey resolves a user-declared "named:<slug>" override for the
// resolved repo path from collector-local config. Returning the bare slug
// (RepoKey adds the "named:" prefix) keeps this seam a drop-in for #78,
// which lands the actual config read; today it always falls through.
func namedRepoKey(_ string) string {
	return ""
}

// gitOriginRemote reads the origin remote URL for the git repository at
// path, or "" if there is none (not a repo, no origin, git missing, or the
// command errors or times out).
func gitOriginRemote(path string) string {
	// #nosec G204 -- git and every option are fixed; path is a single
	// argument to -C and is never interpreted by a shell, matching
	// gitCommonRoot's hardening.
	cmd := exec.Command("git", "-C", path, "remote", "get-url", "origin")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// normalizeRemote turns a git origin remote into a "<host>/<owner>/<name>"
// key, or "" if remote isn't a usable, non-local URL. Host is lowercased;
// owner/name segments are casefolded too so "Acme/API" and "acme/api" -- the
// same GitHub repo, since GitHub is case-preserving but case-insensitive --
// produce the same key. Userinfo (including a token in place of a username)
// and the port are always stripped. A file:// remote or anything that looks
// like a local path is rejected rather than normalized, so a path can never
// reach the wire through this tier; RepoKey falls back to the hashed tier.
func normalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	host, path, ok := parseRemote(remote)
	if !ok || host == "" || path == "" {
		return ""
	}
	key := host + "/" + path
	if looksLikeLocalPath(key) {
		return ""
	}
	return key
}

func parseRemote(remote string) (host, path string, ok bool) {
	// Only try scp-like syntax ([user@]host:path) when remote has no scheme
	// separator: "https://host:port/path" also matches the scp-like shape
	// (host="https", path="//host:port/path") if tried first, since a URL
	// scheme is just another bare "word:" prefix.
	if !strings.Contains(remote, "://") {
		if m := scpLikeRemote.FindStringSubmatch(remote); m != nil {
			return normalizeHostPath(m[1], m[2])
		}
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ssh", "git", "git+ssh":
	default:
		// file:// (typically an empty Host already caught above) and any
		// other scheme is never treated as a usable remote.
		return "", "", false
	}
	return normalizeHostPath(u.Hostname(), u.Path)
}

func normalizeHostPath(hostname, rawPath string) (host, path string, ok bool) {
	host = strings.ToLower(strings.TrimSpace(hostname))
	if host == "" || isDriveLetter(host) {
		return "", "", false
	}
	var segments []string
	for _, seg := range strings.Split(rawPath, "/") {
		if seg = strings.ToLower(strings.TrimSpace(seg)); seg != "" {
			segments = append(segments, seg)
		}
	}
	if len(segments) == 0 {
		return "", "", false
	}
	last := len(segments) - 1
	segments[last] = strings.TrimSuffix(segments[last], ".git")
	if segments[last] == "" {
		return "", "", false
	}
	return host, strings.Join(segments, "/"), true
}

func isDriveLetter(host string) bool {
	return len(host) == 1 && ((host[0] >= 'a' && host[0] <= 'z') || (host[0] >= 'A' && host[0] <= 'Z'))
}

// looksLikeLocalPath is a defense-in-depth check applied to the fully
// normalized remote key: even if parseRemote's rejections above have a gap,
// a key must never carry one of the markers the collector's privacy
// guarantee forbids on the wire (see docs/privacy.md).
func looksLikeLocalPath(key string) bool {
	for _, marker := range []string{"/users/", "/home/", "~"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	segments := strings.SplitN(key, "/", 2)
	return isDriveLetter(segments[0])
}

// fallbackHash returns the first fallbackHashHexLen hex characters of
// sha256(path + salt): the tier-3 body for a path with no usable remote.
func fallbackHash(path string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + repoKeySalt()))
	return hex.EncodeToString(sum[:])[:fallbackHashHexLen]
}
