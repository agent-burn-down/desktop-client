package version

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// LatestReleaseURL is the GitHub API endpoint for this project's latest
// published release.
const LatestReleaseURL = "https://api.github.com/repos/agent-burn-down/" +
	"desktop-client/releases/latest"

// LatestRelease fetches the latest published release tag from the GitHub API at
// url using client. It returns:
//
//   - (tag, nil)  when a release exists,
//   - ("", nil)   when the repository has no releases yet (HTTP 404),
//   - ("", err)   only on transport or decode failure.
//
// Callers treat a 404 (empty tag, nil error) and a transport error as a
// pass-with-note rather than a hard failure, so absence of releases or network
// never fails a health check.
func LatestRelease(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release request returned status %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	return body.TagName, nil
}
