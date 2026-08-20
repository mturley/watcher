package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrAuth indicates that Jira rejected the supplied credentials.
var ErrAuth = errors.New("jira auth failed")

// Validate checks that the given Jira host, email, and API token are valid
// by calling the "myself" endpoint. It returns nil if the credentials are
// valid, an error wrapping ErrAuth if authentication failed (401/403), or a
// plain error for any other failure (network error, non-200 response, or an
// unexpected/empty response body).
func Validate(host, email, token string) error {
	base := strings.TrimRight(host, "/")
	req, err := http.NewRequest(http.MethodGet, base+"/rest/api/3/myself", nil)
	if err != nil {
		return fmt.Errorf("jira validate: %w", err)
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("jira validate request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("invalid Jira credentials: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jira API status %d", resp.StatusCode)
	}

	var out struct {
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("jira validate parse: %w", err)
	}
	if out.DisplayName == "" {
		return fmt.Errorf("jira validate: empty displayName")
	}
	return nil
}
