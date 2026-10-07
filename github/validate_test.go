package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidate(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"viewer":{"login":"me"}}}`))
	}))
	defer okSrv.Close()
	if err := Validate("tok", okSrv.URL); err != nil {
		t.Fatalf("valid: %v", err)
	}
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer authSrv.Close()
	if err := Validate("bad", authSrv.URL); !errors.Is(err, ErrAuth) {
		t.Fatalf("401: want ErrAuth, got %v", err)
	}
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer errSrv.Close()
	if err := Validate("tok", errSrv.URL); err == nil || errors.Is(err, ErrAuth) {
		t.Fatalf("500: want plain error, got %v", err)
	}
}

func TestViewerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":{"viewer":{"databaseId":583231}}}`))
	}))
	defer srv.Close()
	id, err := ViewerID("tok", srv.URL)
	if err != nil || id != "583231" {
		t.Fatalf("ViewerID = %q, %v", id, err)
	}
	if _, err := ViewerID("bad", srv.URL); !errors.Is(err, ErrAuth) {
		t.Fatalf("bad token err = %v, want ErrAuth", err)
	}
}
