package slack

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidateSuccess ensures Validate returns nil when Slack accepts the
// credentials.
func TestValidateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"user":"testuser","team":"testteam"}`))
	}))
	defer srv.Close()

	if err := Validate("xoxc-token", "xoxd-cookie", srv.URL); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestValidateAuthError ensures Validate wraps ErrAuth when Slack rejects the
// credentials.
func TestValidateAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()

	err := Validate("xoxc-token", "xoxd-cookie", srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected wrapped ErrAuth, got: %v", err)
	}
}
