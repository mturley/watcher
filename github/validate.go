package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ErrAuth = errors.New("github auth failed")

func Validate(token string, apiURL ...string) error {
	endpoint := "https://api.github.com/graphql"
	if len(apiURL) > 0 && apiURL[0] != "" {
		endpoint = apiURL[0]
	}
	body, _ := json.Marshal(map[string]string{"query": "{ viewer { login } }"})
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github validate: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("github validate request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("invalid GitHub token: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github API status %d", resp.StatusCode)
	}
	var out struct {
		Data   struct{ Viewer struct{ Login string } }
		Errors []struct{ Message string }
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("github validate parse: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("github GraphQL error: %s", out.Errors[0].Message)
	}
	if out.Data.Viewer.Login == "" {
		return fmt.Errorf("github validate: empty login")
	}
	return nil
}
