//go:build golden

package github

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
	// Create mock GraphQL server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-token" {
			t.Errorf("Expected Authorization header 'Bearer test-token', got %q", authHeader)
		}

		// Return mock response
		response := map[string]interface{}{
			"data": map[string]interface{}{
				"pr0": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"number":    123,
						"state":     "OPEN",
						"title":     "Test PR",
						"updatedAt": "2026-06-17T10:00:00Z",
						"reviews": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"author": map[string]interface{}{
										"__typename": "User",
										"login":      "reviewer1",
									},
									"state":       "APPROVED",
									"submittedAt": "2026-06-17T09:00:00Z",
									"body":        "LGTM",
								},
							},
						},
						"comments": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"author": map[string]interface{}{
										"__typename": "User",
										"login":      "commenter1",
									},
									"createdAt": "2026-06-17T08:00:00Z",
									"body":      "Nice work!",
								},
							},
						},
						"reviewThreads": map[string]interface{}{
							"nodes": []interface{}{},
						},
						"commits": map[string]interface{}{
							"totalCount": 3,
							"nodes": []interface{}{
								map[string]interface{}{
									"commit": map[string]interface{}{
										"oid": "abc123",
									},
								},
							},
						},
						"checkSuites": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"checkRuns": map[string]interface{}{
										"nodes": []interface{}{
											map[string]interface{}{
												"name":        "CI",
												"conclusion":  "SUCCESS",
												"completedAt": "2026-06-17T07:00:00Z",
											},
										},
									},
								},
							},
						},
					},
				},
				"rateLimit": map[string]interface{}{
					"remaining": 4999,
					"limit":     5000,
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
		ResourceType: "pr",
		ResourceID:   "owner/repo#123",
		ResourceURL:  strPtr("https://github.com/owner/repo/pull/123"),
		CreatedAt:    "2026-06-17T06:00:00Z",
	}
	if err := tempDB.Subscribe(sub); err != nil {
		t.Fatalf("Failed to create subscription: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Services: config.Services{
			GitHub: &config.GitHubConfig{
				Token: "test-token",
			},
		},
	}

	// Create test resources
	resources := []watcher.Resource{
		{
			Type: "pr",
			ID:   "owner/repo#123",
			URL:  "https://github.com/owner/repo/pull/123",
		},
	}

	// Create logger (discard output)
	logger := log.New(os.Stderr, "[test] ", log.LstdFlags)

	// Manually fetch and process since Poll doesn't expose API URL
	// Parse PRs
	var prRefs []PRRef
	for _, r := range resources {
		ref, err := ParsePRResourceID(r.ID)
		if err != nil {
			t.Fatalf("Failed to parse PR resource ID: %v", err)
		}
		prRefs = append(prRefs, ref)
	}

	// Fetch using mock server
	prDataList, _, err := FetchPRs(cfg.Services.GitHub.Token, prRefs, server.URL)
	if err != nil {
		t.Fatalf("FetchPRs failed: %v", err)
	}

	// Process the PR data
	if len(prDataList) != 1 {
		t.Fatalf("Expected 1 PR, got %d", len(prDataList))
	}

	_, err = processPR(tempDB, prDataList[0], resources[0], logger)
	if err != nil {
		t.Fatalf("Poll failed: %v", err)
	}

	// Verify events were written
	events, err := tempDB.QueryEvents(db.EventFilter{
		Source: strPtr("github"),
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
			if e.Source != "github" {
				t.Errorf("Expected source 'github', got %q", e.Source)
			}
			if e.Title != "Started watching PR: Test PR" {
				t.Errorf("Expected title 'Started watching PR: Test PR', got %q", e.Title)
			}
		}
	}

	if !foundWatchStarted {
		t.Error("Expected watch_started event, but it was not found")
	}
}

func TestPoll_SubsequentPoll(t *testing.T) {
	// Create mock GraphQL server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"data": map[string]interface{}{
				"pr0": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"number":    123,
						"state":     "OPEN",
						"title":     "Test PR",
						"updatedAt": "2026-06-17T10:00:00Z",
						"reviews": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"author": map[string]interface{}{
										"__typename": "User",
										"login":      "reviewer1",
									},
									"state":       "APPROVED",
									"submittedAt": "2026-06-17T09:00:00Z",
									"body":        "LGTM",
								},
							},
						},
						"comments": map[string]interface{}{
							"nodes": []interface{}{},
						},
						"reviewThreads": map[string]interface{}{
							"nodes": []interface{}{},
						},
						"commits": map[string]interface{}{
							"totalCount": 3,
							"nodes": []interface{}{
								map[string]interface{}{
									"commit": map[string]interface{}{
										"oid": "abc123",
									},
								},
							},
						},
						"checkSuites": map[string]interface{}{
							"nodes": []interface{}{},
						},
					},
				},
				"rateLimit": map[string]interface{}{
					"remaining": 4999,
					"limit":     5000,
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
		ResourceType: "pr",
		ResourceID:   "owner/repo#123",
		ResourceURL:  strPtr("https://github.com/owner/repo/pull/123"),
		CreatedAt:    "2026-06-17T06:00:00Z",
	}
	if err := tempDB.Subscribe(sub); err != nil {
		t.Fatalf("Failed to create subscription: %v", err)
	}

	// Insert a prior event to establish a cursor
	priorEvent := db.Event{
		ID:         uuid.New().String(),
		TS:         "2026-06-17T08:00:00Z",
		ExternalTS: strPtr("2026-06-17T08:00:00Z"),
		Source:     "github",
		SessionID:  nil,
		Type:       "watch_started",
		Title:      "Started watching PR",
		Body:       nil,
		Author:     nil,
		AuthorType: nil,
		Broadcast:  false,
		Tags:       nil,
	}
	priorResources := []db.EventResource{
		{
			ResourceType: "pr",
			ResourceID:   "owner/repo#123",
			ResourceURL:  strPtr("https://github.com/owner/repo/pull/123"),
		},
	}
	if err := tempDB.InsertEvent(priorEvent, nil, priorResources); err != nil {
		t.Fatalf("Failed to insert prior event: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Services: config.Services{
			GitHub: &config.GitHubConfig{
				Token: "test-token",
			},
		},
	}

	// Create test resources
	resources := []watcher.Resource{
		{
			Type: "pr",
			ID:   "owner/repo#123",
			URL:  "https://github.com/owner/repo/pull/123",
		},
	}

	// Create logger
	logger := log.New(os.Stderr, "[test] ", log.LstdFlags)

	// Manually fetch and process since Poll doesn't expose API URL
	// Parse PRs
	var prRefs []PRRef
	for _, r := range resources {
		ref, err := ParsePRResourceID(r.ID)
		if err != nil {
			t.Fatalf("Failed to parse PR resource ID: %v", err)
		}
		prRefs = append(prRefs, ref)
	}

	// Fetch using mock server
	prDataList, _, err := FetchPRs(cfg.Services.GitHub.Token, prRefs, server.URL)
	if err != nil {
		t.Fatalf("FetchPRs failed: %v", err)
	}

	// Process the PR data
	if len(prDataList) != 1 {
		t.Fatalf("Expected 1 PR, got %d", len(prDataList))
	}

	_, err = processPR(tempDB, prDataList[0], resources[0], logger)
	if err != nil {
		t.Fatalf("Poll failed: %v", err)
	}

	// Verify new events were written (should see pr_approved since it's after cursor)
	events, err := tempDB.QueryEvents(db.EventFilter{
		Source: strPtr("github"),
	})
	if err != nil {
		t.Fatalf("Failed to query events: %v", err)
	}

	// Should have at least 2 events: the prior watch_started and the new pr_approved
	if len(events) < 2 {
		t.Fatalf("Expected at least 2 events, got %d", len(events))
	}

	// Verify pr_approved event was emitted
	foundApproved := false
	for _, e := range events {
		if e.Type == "pr_approved" {
			foundApproved = true
			if e.Author == nil || *e.Author != "reviewer1" {
				t.Errorf("Expected author 'reviewer1', got %v", e.Author)
			}
			if e.AuthorType == nil || *e.AuthorType != "user" {
				t.Errorf("Expected author_type 'user', got %v", e.AuthorType)
			}
		}
	}

	if !foundApproved {
		t.Error("Expected pr_approved event, but it was not found")
	}
}

// strPtr is a helper that returns a pointer to a string.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
