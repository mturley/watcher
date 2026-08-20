package jira

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Run("200 with displayName returns nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"displayName":"Me"}`))
		}))
		defer srv.Close()

		if err := Validate(srv.URL, "me@example.com", "token"); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("401 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		err := Validate(srv.URL, "me@example.com", "token")
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("Validate() = %v, want error wrapping ErrAuth", err)
		}
	})

	t.Run("403 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		err := Validate(srv.URL, "me@example.com", "token")
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("Validate() = %v, want error wrapping ErrAuth", err)
		}
	})

	t.Run("500 returns plain error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		err := Validate(srv.URL, "me@example.com", "token")
		if err == nil {
			t.Fatal("Validate() = nil, want error")
		}
		if errors.Is(err, ErrAuth) {
			t.Fatalf("Validate() = %v, want plain error (not ErrAuth)", err)
		}
	})
}
