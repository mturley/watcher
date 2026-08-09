//go:build golden

package jira

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
	"github.com/mturley/watcher/config"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
)

func TestPoll_FirstPoll(t *testing.T) {
	// Create mock Jira server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("Expected GET request, got %s", r.Method)
		}

		username, password, ok := r.BasicAuth()
		if !ok || username != "test@example.com" || password != "test-token" {
			t.Errorf("Expected basic auth test@example.com:test-token, got %s:%s", username, password)
		}

		// Return mock issue response
		response := map[string]interface{}{
			"key": "PROJ-123",
			"fields": map[string]interface{}{
				"summary": "Test Issue",
				"status": map[string]interface{}{
					"name": "In Progress",
				},
				"assignee": map[string]interface{}{
					"displayName": "John Doe",
				},
				"labels":               []string{"bug", "priority"},
				"customfield_12311140": "PROJ-100", // Epic link
				"comment": map[string]interface{}{
					"comments": []interface{}{
						map[string]interface{}{
							"author": map[string]interface{}{
								"displayName": "Jane Smith",
							},
							"created": "2026-06-17T09:00:00.000+0000",
							"body":    map[string]interface{}{}, // ADF body (we ignore it)
						},
					},
				},
			},
			"changelog": map[string]interface{}{
				"histories": []interface{}{
					map[string]interface{}{
						"author": map[string]interface{}{
							"displayName": "John Doe",
						},
						"created": "2026-06-17T08:00:00.000+0000",
						"items": []interface{}{
							map[string]interface{}{
								"field":      "status",
								"fromString": "To Do",
								"toString":   "In Progress",
							},
						},
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create test database
	tempDB := testutil.NewTestDB(t)

	// Create test session
	sessionID := uuid.New().String()
	session := db.Session{
		SessionID:    sessionID,
		Harness:      "claude",
		Repo:         "test-repo",
		Branch:       "main",
		Status:       "active",
		InboxMode:    "manual",
		LastActive:   "2026-06-17T06:00:00Z",
		RegisteredAt: "2026-06-17T06:00:00Z",
		JSONLPath:    "/tmp/test.jsonl",
	}
	if err := tempDB.UpsertSession(session); err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Create test subscription
	sub := db.Subscription{
		ID:           uuid.New().String(),
		SessionID:    sessionID,
		ResourceType: "jira",
		ResourceID:   "PROJ-123",
		ResourceURL:  strPtr("https://redhat.atlassian.net/browse/PROJ-123"),
		CreatedAt:    "2026-06-17T06:00:00Z",
	}
	if err := tempDB.Subscribe(sub); err != nil {
		t.Fatalf("Failed to create subscription: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Services: config.Services{
			Jira: &config.JiraConfig{
				URL:   server.URL,
				Email: "test@example.com",
				Token: "test-token",
			},
		},
	}

	// Create test resources
	resources := []watcher.Resource{
		{
			Type: "jira",
			ID:   "PROJ-123",
			URL:  "https://redhat.atlassian.net/browse/PROJ-123",
		},
	}

	// Create logger
	logger := log.New(os.Stderr, "[test] ", log.LstdFlags)

	// Run poller
	if err := Poll(tempDB, cfg, resources, logger); err != nil {
		t.Fatalf("Poll failed: %v", err)
	}

	// Verify events were written
	events, err := tempDB.QueryEvents(db.EventFilter{
		Source: strPtr("jira"),
	})
	if err != nil {
		t.Fatalf("Failed to query events: %v", err)
	}

	if len(events) == 0 {
		t.Fatal("Expected events to be written, but none were found")
	}

	// Verify watch_started event was emitted (first poll, no cursor)
	foundWatchStarted := false
	for _, e := range events {
		if e.Type == "watch_started" {
			foundWatchStarted = true
			if e.Source != "jira" {
				t.Errorf("Expected source 'jira', got %q", e.Source)
			}
			if e.Title != "Started watching issue: Test Issue" {
				t.Errorf("Expected title 'Started watching issue: Test Issue', got %q", e.Title)
			}
		}
	}

	if !foundWatchStarted {
		t.Error("Expected watch_started event, but it was not found")
	}
}

func TestPoll_SubsequentPoll(t *testing.T) {
	// Create mock Jira server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"key": "PROJ-123",
			"fields": map[string]interface{}{
				"summary": "Test Issue",
				"status": map[string]interface{}{
					"name": "In Progress",
				},
				"assignee": map[string]interface{}{
					"displayName": "John Doe",
				},
				"labels": []string{"bug", "priority"},
				"comment": map[string]interface{}{
					"comments": []interface{}{
						map[string]interface{}{
							"author": map[string]interface{}{
								"displayName": "Jane Smith",
							},
							"created": "2026-06-17T09:00:00.000+0000",
							"body":    map[string]interface{}{},
						},
					},
				},
			},
			"changelog": map[string]interface{}{
				"histories": []interface{}{
					map[string]interface{}{
						"author": map[string]interface{}{
							"displayName": "John Doe",
						},
						"created": "2026-06-17T10:00:00.000+0000",
						"items": []interface{}{
							map[string]interface{}{
								"field":      "status",
								"fromString": "In Progress",
								"toString":   "Done",
							},
						},
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create test database
	tempDB := testutil.NewTestDB(t)

	// Create test session
	sessionID := uuid.New().String()
	session := db.Session{
		SessionID:    sessionID,
		Harness:      "claude",
		Repo:         "test-repo",
		Branch:       "main",
		Status:       "active",
		InboxMode:    "manual",
		LastActive:   "2026-06-17T06:00:00Z",
		RegisteredAt: "2026-06-17T06:00:00Z",
		JSONLPath:    "/tmp/test.jsonl",
	}
	if err := tempDB.UpsertSession(session); err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Create test subscription
	sub := db.Subscription{
		ID:           uuid.New().String(),
		SessionID:    sessionID,
		ResourceType: "jira",
		ResourceID:   "PROJ-123",
		ResourceURL:  strPtr("https://redhat.atlassian.net/browse/PROJ-123"),
		CreatedAt:    "2026-06-17T06:00:00Z",
	}
	if err := tempDB.Subscribe(sub); err != nil {
		t.Fatalf("Failed to create subscription: %v", err)
	}

	// Insert a prior event to establish a cursor
	priorEvent := db.Event{
		ID:         uuid.New().String(),
		TS:         "2026-06-17T08:00:00Z",
		ExternalTS: strPtr("2026-06-17T09:00:00.000+0000"),
		Source:     "jira",
		SessionID:  nil,
		Type:       "watch_started",
		Title:      "Started watching issue",
		Body:       nil,
		Author:     nil,
		AuthorType: nil,
		Broadcast:  false,
		Tags:       nil,
	}
	priorResources := []db.EventResource{
		{
			ResourceType: "jira",
			ResourceID:   "PROJ-123",
			ResourceURL:  strPtr("https://redhat.atlassian.net/browse/PROJ-123"),
		},
	}
	if err := tempDB.InsertEvent(priorEvent, nil, priorResources); err != nil {
		t.Fatalf("Failed to insert prior event: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Services: config.Services{
			Jira: &config.JiraConfig{
				URL:   server.URL,
				Email: "test@example.com",
				Token: "test-token",
			},
		},
	}

	// Create test resources
	resources := []watcher.Resource{
		{
			Type: "jira",
			ID:   "PROJ-123",
			URL:  "https://redhat.atlassian.net/browse/PROJ-123",
		},
	}

	// Create logger
	logger := log.New(os.Stderr, "[test] ", log.LstdFlags)

	// Run poller
	if err := Poll(tempDB, cfg, resources, logger); err != nil {
		t.Fatalf("Poll failed: %v", err)
	}

	// Verify new events were written (should see jira_status_change since it's after cursor)
	events, err := tempDB.QueryEvents(db.EventFilter{
		Source: strPtr("jira"),
	})
	if err != nil {
		t.Fatalf("Failed to query events: %v", err)
	}

	// Should have at least 2 events: the prior watch_started and the new jira_status_change
	if len(events) < 2 {
		t.Fatalf("Expected at least 2 events, got %d", len(events))
	}

	// Verify jira_status_change event was emitted
	foundStatusChange := false
	for _, e := range events {
		if e.Type == "jira_status_change" {
			foundStatusChange = true
			if e.Title != "PROJ-123: In Progress → Done" {
				t.Errorf("Expected title 'PROJ-123: In Progress → Done', got %q", e.Title)
			}
			if e.Author == nil || *e.Author != "John Doe" {
				t.Errorf("Expected author 'John Doe', got %v", e.Author)
			}
		}
	}

	if !foundStatusChange {
		t.Error("Expected jira_status_change event, but it was not found")
	}
}

func TestDeduplication(t *testing.T) {
	// Create mock Jira server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"key": "PROJ-123",
			"fields": map[string]interface{}{
				"summary": "Test Issue",
				"status": map[string]interface{}{
					"name": "In Progress",
				},
				"labels": []string{},
				"comment": map[string]interface{}{
					"comments": []interface{}{
						map[string]interface{}{
							"author": map[string]interface{}{
								"displayName": "Jane Smith",
							},
							"created": "2026-06-17T09:00:00.000+0000",
							"body":    map[string]interface{}{},
						},
					},
				},
			},
			"changelog": map[string]interface{}{
				"histories": []interface{}{},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create test database
	tempDB := testutil.NewTestDB(t)

	// Create test session
	sessionID := uuid.New().String()
	session := db.Session{
		SessionID:    sessionID,
		Harness:      "claude",
		Repo:         "test-repo",
		Branch:       "main",
		Status:       "active",
		InboxMode:    "manual",
		LastActive:   "2026-06-17T06:00:00Z",
		RegisteredAt: "2026-06-17T06:00:00Z",
		JSONLPath:    "/tmp/test.jsonl",
	}
	if err := tempDB.UpsertSession(session); err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Create test subscription
	sub := db.Subscription{
		ID:           uuid.New().String(),
		SessionID:    sessionID,
		ResourceType: "jira",
		ResourceID:   "PROJ-123",
		ResourceURL:  strPtr("https://redhat.atlassian.net/browse/PROJ-123"),
		CreatedAt:    "2026-06-17T06:00:00Z",
	}
	if err := tempDB.Subscribe(sub); err != nil {
		t.Fatalf("Failed to create subscription: %v", err)
	}

	// Insert a prior comment event (same timestamp as the comment we'll fetch)
	priorEvent := db.Event{
		ID:         uuid.New().String(),
		TS:         "2026-06-17T08:00:00Z",
		ExternalTS: strPtr("2026-06-17T09:00:00.000+0000"),
		Source:     "jira",
		SessionID:  nil,
		Type:       "jira_comment",
		Title:      "Comment by Jane Smith on PROJ-123",
		Body:       nil,
		Author:     strPtr("Jane Smith"),
		AuthorType: strPtr("human"),
		Broadcast:  false,
		Tags:       nil,
	}
	priorResources := []db.EventResource{
		{
			ResourceType: "jira",
			ResourceID:   "PROJ-123",
			ResourceURL:  strPtr("https://redhat.atlassian.net/browse/PROJ-123"),
		},
	}
	if err := tempDB.InsertEvent(priorEvent, nil, priorResources); err != nil {
		t.Fatalf("Failed to insert prior event: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Services: config.Services{
			Jira: &config.JiraConfig{
				URL:   server.URL,
				Email: "test@example.com",
				Token: "test-token",
			},
		},
	}

	// Create test resources
	resources := []watcher.Resource{
		{
			Type: "jira",
			ID:   "PROJ-123",
			URL:  "https://redhat.atlassian.net/browse/PROJ-123",
		},
	}

	// Create logger
	logger := log.New(os.Stderr, "[test] ", log.LstdFlags)

	// Run poller twice
	if err := Poll(tempDB, cfg, resources, logger); err != nil {
		t.Fatalf("First poll failed: %v", err)
	}
	if err := Poll(tempDB, cfg, resources, logger); err != nil {
		t.Fatalf("Second poll failed: %v", err)
	}

	// Verify only one jira_comment event exists (deduplication worked)
	events, err := tempDB.QueryEvents(db.EventFilter{
		Source: strPtr("jira"),
		Type:   strPtr("jira_comment"),
	})
	if err != nil {
		t.Fatalf("Failed to query events: %v", err)
	}

	if len(events) != 1 {
		t.Errorf("Expected exactly 1 jira_comment event (deduplication), got %d", len(events))
	}
}

// strPtr is a helper that returns a pointer to a string.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
