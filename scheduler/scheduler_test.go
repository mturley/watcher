package scheduler

import (
	"strings"
	"testing"
	"time"
)

func testConfig() ScheduleConfig {
	return ScheduleConfig{
		Name:        "github",
		LabelPrefix: "com.example",
		Command:     []string{"/usr/local/bin/mytool", "poll", "github"},
		Interval:    3 * time.Minute,
		LogPath:     "/tmp/x.log",
	}
}

func TestBuildPlist(t *testing.T) {
	cfg := testConfig()

	plist, err := buildPlist(cfg)
	if err != nil {
		t.Fatalf("buildPlist returned error: %v", err)
	}

	if !strings.Contains(plist, "<string>com.example.watcher-github</string>") {
		t.Errorf("expected plist to contain label com.example.watcher-github, got:\n%s", plist)
	}

	for _, arg := range cfg.Command {
		want := "<string>" + arg + "</string>"
		if !strings.Contains(plist, want) {
			t.Errorf("expected plist to contain %q, got:\n%s", want, plist)
		}
	}

	if !strings.Contains(plist, "<integer>180</integer>") {
		t.Errorf("expected plist to contain StartInterval 180, got:\n%s", plist)
	}

	if !strings.Contains(plist, "<string>/tmp/x.log</string>") {
		t.Errorf("expected plist to contain log path /tmp/x.log, got:\n%s", plist)
	}
}

func TestBuildCronEntry(t *testing.T) {
	cfg := testConfig()

	entry, err := buildCronEntry(cfg)
	if err != nil {
		t.Fatalf("buildCronEntry returned error: %v", err)
	}

	wantCommand := strings.Join(cfg.Command, " ")
	if !strings.Contains(entry, wantCommand) {
		t.Errorf("expected cron entry to contain joined command %q, got:\n%s", wantCommand, entry)
	}

	if !strings.Contains(entry, cfg.LogPath) {
		t.Errorf("expected cron entry to contain log path %q, got:\n%s", cfg.LogPath, entry)
	}

	if !strings.Contains(entry, "*/3 * * * *") {
		t.Errorf("expected cron entry to contain */3 minute schedule, got:\n%s", entry)
	}

	if !strings.Contains(entry, "# com.example-watcher-github") {
		t.Errorf("expected cron entry to contain marker # com.example-watcher-github, got:\n%s", entry)
	}
}

func TestBuildCronEntryRoundsUpFractionalMinutes(t *testing.T) {
	cfg := testConfig()
	cfg.Interval = 90 * time.Second // not a whole number of minutes

	entry, err := buildCronEntry(cfg)
	if err != nil {
		t.Fatalf("buildCronEntry returned error: %v", err)
	}

	if !strings.Contains(entry, "*/2 * * * *") {
		t.Errorf("expected cron entry to round up to */2 minute schedule, got:\n%s", entry)
	}
}

func TestBuildPlistValidation(t *testing.T) {
	cfg := testConfig()
	cfg.Name = ""

	if _, err := buildPlist(cfg); err == nil {
		t.Error("expected buildPlist to error on missing Name")
	}
}

func TestBuildCronEntryValidation(t *testing.T) {
	cfg := testConfig()
	cfg.Command = nil

	if _, err := buildCronEntry(cfg); err == nil {
		t.Error("expected buildCronEntry to error on missing Command")
	}
}
