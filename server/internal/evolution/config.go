// Added by the mouse9527/multica fork: Evolution P2, configuration (M-136).

package evolution

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// WorkspaceSettingKey is the key of the Evolution configuration inside
// workspace.settings. Nothing outside this package reads it.
const WorkspaceSettingKey = "evolution"

// Server-enforced bounds. A stored value outside them, however it got there,
// is clamped when read; a PUT outside them is rejected.
const (
	MinItemsPerWindow     = 50
	MaxItemsPerWindow     = 5000
	DefaultItemsPerWindow = 2000

	MinWindowDays     = 1
	MaxWindowDays     = 30
	DefaultWindowDays = 7

	MinRunsPerDay     = 1
	MaxRunsPerDay     = 10
	DefaultRunsPerDay = 3

	MinIntervalDays = 1
	MaxIntervalDays = 7

	MaxExcluded = 200

	DefaultScheduleTime     = "03:00"
	DefaultScheduleTimezone = "Asia/Shanghai"
)

// Config is a workspace's Evolution configuration. It is stored whole under
// workspace.settings.evolution and replaced whole by PUT /api/evolution/config.
type Config struct {
	// Enabled turns the scheduled analysis on. The agent itself is enabled
	// separately (POST /api/evolution/enable).
	Enabled  bool     `json:"enabled"`
	Schedule Schedule `json:"schedule"`
	Sources  Sources  `json:"sources"`
	// ExcludeProjects and ExcludeAgents are never read: comments on issues in
	// these projects, comments these agents wrote, chats with these agents, and
	// chats filed under these projects.
	ExcludeProjects []string `json:"exclude_projects"`
	ExcludeAgents   []string `json:"exclude_agents"`
	// MaxItemsPerWindow caps what one run may read across sources; the rest
	// is left for a catch-up run.
	MaxItemsPerWindow int `json:"max_items_per_window"`
	// MaxWindowDays caps how far past its cursor one run reads a source.
	MaxWindowDays int `json:"max_window_days"`
	// MaxRunsPerDay caps scheduled plus catch-up runs per local day.
	MaxRunsPerDay int `json:"max_runs_per_day"`
}

// Schedule is when the daily analysis starts: at Time in Timezone, every
// IntervalDays days.
type Schedule struct {
	Time         string `json:"time"`
	Timezone     string `json:"timezone"`
	IntervalDays int    `json:"interval_days"`
}

// Sources are the data sources an analysis reads. Chat is read only after
// members were told (see SetConfig); the flag alone does not turn it on.
type Sources struct {
	Comments bool `json:"comments"`
	Chat     bool `json:"chat"`
}

// DefaultConfig is the configuration of a workspace that never saved one.
// Chat starts off: turning it on is what sends the member notice.
func DefaultConfig() Config {
	return Config{
		Schedule: Schedule{
			Time:         DefaultScheduleTime,
			Timezone:     DefaultScheduleTimezone,
			IntervalDays: 1,
		},
		Sources:           Sources{Comments: true},
		ExcludeProjects:   []string{},
		ExcludeAgents:     []string{},
		MaxItemsPerWindow: DefaultItemsPerWindow,
		MaxWindowDays:     DefaultWindowDays,
		MaxRunsPerDay:     DefaultRunsPerDay,
	}
}

// Limits describes the bounds for clients.
type Limits struct {
	ItemsPerWindow [2]int `json:"items_per_window"`
	WindowDays     [2]int `json:"window_days"`
	RunsPerDay     [2]int `json:"runs_per_day"`
	IntervalDays   [2]int `json:"interval_days"`
	MaxExcluded    int    `json:"max_excluded"`
}

func CurrentLimits() Limits {
	return Limits{
		ItemsPerWindow: [2]int{MinItemsPerWindow, MaxItemsPerWindow},
		WindowDays:     [2]int{MinWindowDays, MaxWindowDays},
		RunsPerDay:     [2]int{MinRunsPerDay, MaxRunsPerDay},
		IntervalDays:   [2]int{MinIntervalDays, MaxIntervalDays},
		MaxExcluded:    MaxExcluded,
	}
}

var scheduleTimePattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// ConfigFromSettings reads the configuration out of workspace.settings. A
// missing or malformed key yields the defaults; stored values are clamped to
// the bounds, because workspace.settings can also be written whole through
// the upstream workspace update.
func ConfigFromSettings(settings []byte) Config {
	cfg := DefaultConfig()
	var all map[string]json.RawMessage
	if len(settings) == 0 || json.Unmarshal(settings, &all) != nil {
		return cfg
	}
	raw, ok := all[WorkspaceSettingKey]
	if !ok || string(raw) == "null" {
		return cfg
	}
	stored := DefaultConfig()
	if json.Unmarshal(raw, &stored) != nil {
		return cfg
	}
	return stored.clamped()
}

func (c Config) clamped() Config {
	d := DefaultConfig()
	c.MaxItemsPerWindow = clampInt(c.MaxItemsPerWindow, MinItemsPerWindow, MaxItemsPerWindow)
	c.MaxWindowDays = clampInt(c.MaxWindowDays, MinWindowDays, MaxWindowDays)
	c.MaxRunsPerDay = clampInt(c.MaxRunsPerDay, MinRunsPerDay, MaxRunsPerDay)
	c.Schedule.IntervalDays = clampInt(c.Schedule.IntervalDays, MinIntervalDays, MaxIntervalDays)
	if !scheduleTimePattern.MatchString(c.Schedule.Time) {
		c.Schedule.Time = d.Schedule.Time
	}
	if _, err := time.LoadLocation(c.Schedule.Timezone); err != nil || c.Schedule.Timezone == "" {
		c.Schedule.Timezone = d.Schedule.Timezone
	}
	c.ExcludeProjects = validUUIDs(c.ExcludeProjects)
	c.ExcludeAgents = validUUIDs(c.ExcludeAgents)
	return c
}

// InvalidConfigError is a configuration outside the bounds; its message is
// client-facing.
type InvalidConfigError struct{ Message string }

func (e *InvalidConfigError) Error() string { return e.Message }

// IsInvalidConfig reports whether err is an InvalidConfigError.
func IsInvalidConfig(err error) bool {
	var e *InvalidConfigError
	return errors.As(err, &e)
}

func invalidf(format string, args ...any) error {
	return &InvalidConfigError{Message: fmt.Sprintf(format, args...)}
}

// Validate rejects a configuration a client sent that is outside the bounds.
// It returns a client-facing message.
func (c Config) Validate() error {
	if c.MaxItemsPerWindow < MinItemsPerWindow || c.MaxItemsPerWindow > MaxItemsPerWindow {
		return invalidf("max_items_per_window must be between %d and %d", MinItemsPerWindow, MaxItemsPerWindow)
	}
	if c.MaxWindowDays < MinWindowDays || c.MaxWindowDays > MaxWindowDays {
		return invalidf("max_window_days must be between %d and %d", MinWindowDays, MaxWindowDays)
	}
	if c.MaxRunsPerDay < MinRunsPerDay || c.MaxRunsPerDay > MaxRunsPerDay {
		return invalidf("max_runs_per_day must be between %d and %d", MinRunsPerDay, MaxRunsPerDay)
	}
	if c.Schedule.IntervalDays < MinIntervalDays || c.Schedule.IntervalDays > MaxIntervalDays {
		return invalidf("schedule.interval_days must be between %d and %d", MinIntervalDays, MaxIntervalDays)
	}
	if !scheduleTimePattern.MatchString(c.Schedule.Time) {
		return invalidf("schedule.time must be HH:MM (24-hour)")
	}
	if c.Schedule.Timezone == "" {
		return invalidf("schedule.timezone is required")
	}
	if _, err := time.LoadLocation(c.Schedule.Timezone); err != nil {
		return invalidf("schedule.timezone %q is not a known time zone", c.Schedule.Timezone)
	}
	for field, ids := range map[string][]string{"exclude_projects": c.ExcludeProjects, "exclude_agents": c.ExcludeAgents} {
		if len(ids) > MaxExcluded {
			return invalidf("%s may list at most %d ids", field, MaxExcluded)
		}
		for _, id := range ids {
			if _, err := util.ParseUUID(id); err != nil {
				return invalidf("%s: %q is not a uuid", field, id)
			}
		}
	}
	return nil
}

// normalized de-duplicates and lower-cases the id lists so a stored config
// compares and reads predictably.
func (c Config) normalized() Config {
	c.ExcludeProjects = validUUIDs(c.ExcludeProjects)
	c.ExcludeAgents = validUUIDs(c.ExcludeAgents)
	return c
}

// ChatWanted reports whether the configuration asks for chats to be read.
// Reading also needs the member notice (the chat cursor row).
func (c Config) ChatWanted() bool { return c.Enabled && c.Sources.Chat }

// CommentsWanted reports whether comments are read.
func (c Config) CommentsWanted() bool { return c.Enabled && c.Sources.Comments }

// Location is the schedule's time zone; ConfigFromSettings and Validate
// guarantee it loads.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Schedule.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func validUUIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if _, err := util.ParseUUID(id); err != nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) == MaxExcluded {
			break
		}
	}
	return out
}
