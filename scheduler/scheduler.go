// Package scheduler generates and manages OS-level periodic scheduling for a
// watcher: a launchd plist on macOS, or a crontab entry on Linux. The
// scheduler has no knowledge of any particular binary — the consumer supplies
// the full command to run via ScheduleConfig.Command.
package scheduler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"
)

// ScheduleConfig describes a watcher to schedule.
type ScheduleConfig struct {
	// Name is the logical name of the watcher, e.g. "github".
	Name string
	// LabelPrefix is a reverse-DNS-ish prefix used to build the launchd
	// label and the cron marker comment, e.g. "com.mytool" or "handler".
	LabelPrefix string
	// Command is the full argv to run, e.g.
	// []string{"/usr/local/bin/handler", "watcher", "run", "github"}.
	// The consumer supplies the binary path and any arguments.
	Command []string
	// Interval is the polling interval.
	Interval time.Duration
	// LogPath is the stdout/stderr log path.
	LogPath string
}

func (c ScheduleConfig) validate() error {
	if c.Name == "" {
		return fmt.Errorf("scheduler: Name is required")
	}
	if c.LabelPrefix == "" {
		return fmt.Errorf("scheduler: LabelPrefix is required")
	}
	if len(c.Command) == 0 {
		return fmt.Errorf("scheduler: Command is required")
	}
	if c.LogPath == "" {
		return fmt.Errorf("scheduler: LogPath is required")
	}
	return nil
}

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{range .Command}}		<string>{{.}}</string>
{{end}}	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StartInterval</key>
	<integer>{{.IntervalSeconds}}</integer>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`

type plistData struct {
	Label           string
	Command         []string
	IntervalSeconds int
	LogPath         string
}

// launchdLabel derives the launchd label for a watcher: <labelPrefix>.watcher-<name>.
func launchdLabel(labelPrefix, name string) string {
	return fmt.Sprintf("%s.watcher-%s", labelPrefix, name)
}

// cronMarker derives the cron comment marker for a watcher: # <labelPrefix>-watcher-<name>.
func cronMarker(labelPrefix, name string) string {
	return fmt.Sprintf("# %s-watcher-%s", labelPrefix, name)
}

// cronStoppedMarker derives the marker used while a cron entry is stopped.
func cronStoppedMarker(labelPrefix, name string) string {
	return cronMarker(labelPrefix, name) + "-stopped"
}

// intervalMinutes converts an interval to whole minutes for cron, rounding up
// to at least 1 minute.
func intervalMinutes(interval time.Duration) int {
	minutes := int(interval / time.Minute)
	if interval%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		minutes = 1
	}
	return minutes
}

// buildPlist renders the launchd plist XML for cfg. It is a pure function
// with no filesystem or exec side effects.
func buildPlist(cfg ScheduleConfig) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}

	tmpl, err := template.New("plist").Parse(launchdPlistTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse plist template: %w", err)
	}

	data := plistData{
		Label:           launchdLabel(cfg.LabelPrefix, cfg.Name),
		Command:         cfg.Command,
		IntervalSeconds: int(cfg.Interval / time.Second),
		LogPath:         cfg.LogPath,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute plist template: %w", err)
	}
	return buf.String(), nil
}

// buildCronEntry renders the cron marker comment and schedule line for cfg,
// joined by a newline. It is a pure function with no filesystem or exec side
// effects.
func buildCronEntry(cfg ScheduleConfig) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}

	minutes := intervalMinutes(cfg.Interval)
	cronSchedule := fmt.Sprintf("*/%d * * * *", minutes)
	commandLine := strings.Join(cfg.Command, " ")
	marker := cronMarker(cfg.LabelPrefix, cfg.Name)

	return fmt.Sprintf("%s\n%s %s >> %s 2>&1", marker, cronSchedule, commandLine, cfg.LogPath), nil
}

// Install installs a scheduled watcher on the current platform. On macOS,
// creates and loads a launchd plist. On Linux, adds a cron entry.
func Install(cfg ScheduleConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return installLaunchd(cfg)
	}
	return installCron(cfg)
}

// Uninstall removes the scheduled watcher from the current platform.
func Uninstall(name, labelPrefix string) error {
	if runtime.GOOS == "darwin" {
		return uninstallLaunchd(name, labelPrefix)
	}
	return uninstallCron(name, labelPrefix)
}

// Stop pauses a watcher without removing it. The plist/cron entry remains but is unloaded.
func Stop(name, labelPrefix string) error {
	installed, err := IsInstalled(name, labelPrefix)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("watcher %q is not installed", name)
	}
	if runtime.GOOS == "darwin" {
		plistPath, err := launchdPlistPath(name, labelPrefix)
		if err != nil {
			return err
		}
		exec.Command("launchctl", "unload", plistPath).Run()
		return nil
	}
	return stopCron(name, labelPrefix)
}

// Start resumes a stopped watcher.
func Start(name, labelPrefix string) error {
	installed, err := IsInstalled(name, labelPrefix)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("watcher %q is not installed", name)
	}
	if runtime.GOOS == "darwin" {
		plistPath, err := launchdPlistPath(name, labelPrefix)
		if err != nil {
			return err
		}
		return exec.Command("launchctl", "load", plistPath).Run()
	}
	return startCron(name, labelPrefix)
}

// IsRunning checks if the watcher is actively scheduled (installed and not stopped).
func IsRunning(name, labelPrefix string) (bool, error) {
	installed, err := IsInstalled(name, labelPrefix)
	if err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	if runtime.GOOS == "darwin" {
		label := launchdLabel(labelPrefix, name)
		output, err := exec.Command("launchctl", "list", label).CombinedOutput()
		return err == nil && len(output) > 0, nil
	}
	return isRunningCron(name, labelPrefix)
}

// IsInstalled checks if the watcher is installed on the current platform.
func IsInstalled(name, labelPrefix string) (bool, error) {
	if runtime.GOOS == "darwin" {
		return isInstalledLaunchd(name, labelPrefix)
	}
	return isInstalledCron(name, labelPrefix)
}

// InstalledInterval returns the configured polling interval in seconds for
// an installed watcher. Returns 0 if the watcher is not installed or the
// interval can't be determined.
func InstalledInterval(name, labelPrefix string) int {
	if runtime.GOOS == "darwin" {
		plistPath, err := launchdPlistPath(name, labelPrefix)
		if err != nil {
			return 0
		}
		data, err := os.ReadFile(plistPath)
		if err != nil {
			return 0
		}
		content := string(data)
		// Parse <key>StartInterval</key>\n\t<integer>N</integer>
		idx := strings.Index(content, "<key>StartInterval</key>")
		if idx < 0 {
			return 0
		}
		rest := content[idx:]
		start := strings.Index(rest, "<integer>")
		end := strings.Index(rest, "</integer>")
		if start < 0 || end < 0 {
			return 0
		}
		var interval int
		fmt.Sscanf(rest[start+len("<integer>"):end], "%d", &interval)
		return interval
	}
	// For cron, intervals are in minutes and harder to parse generically.
	return 0
}

// installLaunchd creates and loads a launchd plist for the watcher.
func installLaunchd(cfg ScheduleConfig) error {
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	plistContent, err := buildPlist(cfg)
	if err != nil {
		return err
	}

	plistPath, err := launchdPlistPath(cfg.Name, cfg.LabelPrefix)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	cmd := exec.Command("launchctl", "load", plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to load plist: %w (output: %s)", err, output)
	}

	return nil
}

// uninstallLaunchd removes the launchd plist for the watcher.
func uninstallLaunchd(name, labelPrefix string) error {
	plistPath, err := launchdPlistPath(name, labelPrefix)
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("watcher %q is not installed", name)
	}

	cmd := exec.Command("launchctl", "unload", plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Don't fail if unload errors - the job might not be loaded.
		_ = output
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("failed to remove plist: %w", err)
	}

	return nil
}

// isInstalledLaunchd checks if the launchd plist exists.
func isInstalledLaunchd(name, labelPrefix string) (bool, error) {
	plistPath, err := launchdPlistPath(name, labelPrefix)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(plistPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// launchdPlistPath returns the path to the launchd plist for the given watcher name.
func launchdPlistPath(name, labelPrefix string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(labelPrefix, name)+".plist"), nil
}

// installCron adds a cron entry for the watcher.
func installCron(cfg ScheduleConfig) error {
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	entry, err := buildCronEntry(cfg)
	if err != nil {
		return err
	}

	// Read existing crontab.
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	existingCrontab := ""
	if err == nil {
		existingCrontab = string(output)
	}

	// Filter out any existing entry for this watcher.
	filtered := filterCronLines(existingCrontab, cronMarker(cfg.LabelPrefix, cfg.Name))
	filtered = append(filtered, entry)

	newCrontab := strings.Join(filtered, "\n") + "\n"
	cmd = exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(newCrontab)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to install crontab: %w (output: %s)", err, output)
	}

	return nil
}

// uninstallCron removes the cron entry for the watcher.
func uninstallCron(name, labelPrefix string) error {
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to read crontab: %w", err)
	}

	existingCrontab := string(output)
	marker := cronMarker(labelPrefix, name)
	found := strings.Contains(existingCrontab, marker)
	if !found {
		return fmt.Errorf("watcher %q is not installed", name)
	}

	filtered := filterCronLines(existingCrontab, marker)

	newCrontab := strings.Join(filtered, "\n")
	if strings.TrimSpace(newCrontab) != "" {
		newCrontab += "\n"
	}
	cmd = exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(newCrontab)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update crontab: %w (output: %s)", err, output)
	}

	return nil
}

// filterCronLines returns the lines of crontab with the marker's comment and
// its following schedule line removed.
func filterCronLines(crontab, marker string) []string {
	lines := strings.Split(crontab, "\n")
	var filtered []string
	skipNext := false
	for _, line := range lines {
		if strings.TrimSpace(line) == marker {
			skipNext = true
			continue
		}
		if skipNext {
			skipNext = false
			continue
		}
		if strings.TrimSpace(line) != "" {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

// isInstalledCron checks if a cron entry exists for the watcher.
func isInstalledCron(name, labelPrefix string) (bool, error) {
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	return strings.Contains(string(output), cronMarker(labelPrefix, name)), nil
}

func stopCron(name, labelPrefix string) error {
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to read crontab: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	marker := cronMarker(labelPrefix, name)
	stoppedMarker := cronStoppedMarker(labelPrefix, name)
	var result []string
	for _, line := range lines {
		if strings.TrimSpace(line) == marker {
			result = append(result, stoppedMarker)
			continue
		}
		result = append(result, line)
	}

	newCrontab := strings.Join(result, "\n")
	cmd = exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(newCrontab)
	_, err = cmd.CombinedOutput()
	return err
}

func startCron(name, labelPrefix string) error {
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to read crontab: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	marker := cronMarker(labelPrefix, name)
	stoppedMarker := cronStoppedMarker(labelPrefix, name)
	var result []string
	for _, line := range lines {
		if strings.TrimSpace(line) == stoppedMarker {
			result = append(result, marker)
			continue
		}
		result = append(result, line)
	}

	newCrontab := strings.Join(result, "\n")
	cmd = exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(newCrontab)
	_, err = cmd.CombinedOutput()
	return err
}

func isRunningCron(name, labelPrefix string) (bool, error) {
	cmd := exec.Command("crontab", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	marker := cronMarker(labelPrefix, name)
	stoppedMarker := cronStoppedMarker(labelPrefix, name)
	content := string(output)
	return strings.Contains(content, marker) && !strings.Contains(content, stoppedMarker), nil
}
