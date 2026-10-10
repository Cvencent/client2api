// Command client2api is the single entry point.  It does wiring and nothing
// else: every vendor decision lives behind core.Client in a clients/* module.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"client2api/internal/alerts"
	"client2api/internal/core"
	"client2api/internal/gateway"
	"client2api/internal/livecfg"
	"client2api/internal/logfile"
	"client2api/internal/modelmeta"
	"client2api/internal/panel"
	"client2api/internal/scheduler"
	"client2api/internal/tray"

	// The only place that knows the full client list.  Adding a client means
	// adding one line to clients/all, never editing an existing module.
	_ "client2api/clients/all"
)

// version is overridable with -ldflags "-X main.version=...".
var version = "0.1.34"

// restartHandoffEnv marks the replacement half of a panel restart.  It tells a
// starting process to keep retrying the listen address instead of failing fast,
// because the process it is replacing may still be holding the port for a few
// hundred milliseconds while its HTTP response drains.
const restartHandoffEnv = "CLIENT2API_RESTART_HANDOFF"

// ---------------------------------------------------------------------------
// configuration
//
// The section names and defaults mirror the reference configuration file so
// that an operator's existing config keeps its meaning.  Every section is
// optional: its zero value selects the documented default.
// ---------------------------------------------------------------------------

type fileConfig struct {
	Listen      string                       `json:"listen"`
	APIKey      string                       `json:"api_key"`
	DataDir     string                       `json:"data_dir"`
	Proxy       string                       `json:"proxy"`
	Aliases     map[string]string            `json:"aliases"`
	Disabled    []string                     `json:"disabled"`
	Platforms   map[string]platformConfig    `json:"platforms"`
	ModelGroups map[string]modelGroupConfig  `json:"model_groups"`
	Clients     map[string]json.RawMessage   `json:"clients"`
	Sources     map[string]core.SourceConfig `json:"sources,omitempty"`

	Schedule      scheduleConfig      `json:"schedule"`
	Prompt        promptConfig        `json:"prompt"`
	SessionSticky sessionStickyConfig `json:"session_sticky"`
	Pool          poolConfig          `json:"pool"`
	Cooldown      cooldownConfig      `json:"cooldown"`
	Features      featuresConfig      `json:"features"`
	Panel         panelConfig         `json:"panel"`
}

// platformConfig is the per-platform routing policy as written in the config
// file: the priority used when several platforms serve the same bare model id,
// and the upstream models this platform must not be given.  DisabledModels is
// a blacklist, so a model that is not listed stays callable.
type platformConfig struct {
	Priority         int                    `json:"priority"`
	PrioritySchedule []priorityWindowConfig `json:"priority_schedule,omitempty"`
	DisabledModels   []string               `json:"disabled_models"`
	// MaxInFlight caps concurrent requests against this platform.  Absent
	// means the default of 2; an explicit 0 means no ceiling.  A pointer is
	// required so the two cases stay distinguishable in JSON.
	MaxInFlight *int `json:"max_in_flight"`
	// MaxInFlightPerAccount caps concurrent requests against any one account
	// of this platform.  Absent means the default of 2; an explicit 0 means
	// no ceiling.  When one account is full the request moves to another
	// account, and only when every account is full does the platform report
	// busy.
	MaxInFlightPerAccount *int `json:"max_in_flight_per_account"`
	// ReserveCredits is the low-balance guard: an account whose last known
	// balance is at or below it is parked until the balance rises again.
	// 0 (the default) parks a known zero balance; a negative value disables
	// the guard for this platform.
	ReserveCredits int `json:"reserve_credits"`
	// AccountPriorities maps account id -> routing priority.  Lower numbers
	// are tried first.  Accounts in the same tier keep their normal
	// round-robin/weighted rotation.
	AccountPriorities map[string]int `json:"account_priorities,omitempty"`
	// AccountNotes maps account id -> the operator's own label for that
	// account (the phone number or e-mail it signs in with).  The panel
	// shows it beside the account so a broken credential can be signed in
	// again as the right identity.  It is display metadata, not routing.
	AccountNotes map[string]string `json:"account_notes,omitempty"`
}

// modelGroupConfig is one operator-declared equivalence group in the config
// file. Members use the qualified client/model spelling; the projection
// splits them into the registry's structured type.
type modelGroupConfig struct {
	Members            []string       `json:"members"`
	PlatformPriorities map[string]int `json:"platform_priorities,omitempty"`
}

// builtinGPT6ModelGroups are native, lowercase GPT-6+ names that Codex expects
// to see without a platform prefix. They route through the same equivalence
// tables as operator-declared groups, while a config entry with the same name
// replaces the default and an explicit empty entry suppresses it.
var builtinGPT6ModelGroups = map[string][]string{
	"gpt-6-astra":     {"Auto/gpt-6-astra", "opencode/gpt-6-astra"},
	"gpt-6.1-sol":     {"Auto/gpt-6.1-sol", "opencode/gpt-6.1-sol"},
	"gpt-6-sol":       {"Auto/gpt-6-sol", "opencode/gpt-6-sol", "openrouter/openai/gpt-6-sol"},
	"gpt-6-luna":      {"Auto/gpt-6-luna", "opencode/gpt-6-luna", "openrouter/openai/gpt-6-luna"},
	"gpt-6.1-sol-pro": {"Auto/gpt-6.1-sol-pro", "openrouter/openai/gpt-6.1-sol-pro"},
}

// platformConfigs projects the file's platforms block onto the router's own
// policy type.  A blank model id is dropped: it can never match an upstream id
// and would only be a dead blacklist entry.  A missing block projects an empty
// map, which installs the documented default everywhere.
func (c *fileConfig) platformConfigs() map[string]core.PlatformConfig {
	return c.platformConfigsFor(nil)
}

// platformConfigsFor is platformConfigs plus the set of live clients.  A
// platform the file never mentions still receives the documented default
// ceilings; passing nil (unit tests, callers with no registry yet) keeps the
// original file-only projection.
func (c *fileConfig) platformConfigsFor(clients []core.Client) map[string]core.PlatformConfig {
	out := make(map[string]core.PlatformConfig, len(c.Platforms)+len(clients))
	for _, client := range clients {
		if client == nil || strings.TrimSpace(client.Name()) == "" {
			continue
		}
		name := client.Name()
		if _, ok := out[name]; !ok {
			out[name] = core.PlatformConfig{
				MaxInFlight:           defaultPlatformMaxInFlight,
				MaxInFlightPerAccount: defaultPlatformMaxInFlightPerAccount,
			}
		}
	}
	for name, p := range c.Platforms {
		cfg := core.PlatformConfig{Priority: p.Priority, PrioritySchedule: projectPrioritySchedule(p.PrioritySchedule)}
		cfg.MaxInFlight = intOr(p.MaxInFlight, defaultPlatformMaxInFlight)
		cfg.MaxInFlightPerAccount = intOr(p.MaxInFlightPerAccount, defaultPlatformMaxInFlightPerAccount)
		cfg.ReserveCredits = p.ReserveCredits
		if len(p.AccountPriorities) > 0 {
			cfg.AccountPriorities = make(map[string]int, len(p.AccountPriorities))
			for id, priority := range p.AccountPriorities {
				if strings.TrimSpace(id) != "" {
					cfg.AccountPriorities[id] = priority
				}
			}
		}
		if len(p.AccountNotes) > 0 {
			cfg.AccountNotes = make(map[string]string, len(p.AccountNotes))
			for id, note := range p.AccountNotes {
				if strings.TrimSpace(id) != "" && strings.TrimSpace(note) != "" {
					cfg.AccountNotes[id] = strings.TrimSpace(note)
				}
			}
		}
		for _, m := range p.DisabledModels {
			if strings.TrimSpace(m) != "" {
				cfg.DisabledModels = append(cfg.DisabledModels, m)
			}
		}
		out[name] = cfg
	}
	return out
}

// modelGroups projects the file's model_groups block onto the router's
// equivalence type. Blank names and malformed member rows are ignored here;
// the panel validates operator writes before they reach this path.
func (c *fileConfig) modelGroups() map[string]core.ModelGroup {
	rawGroups := make(map[string]modelGroupConfig, len(builtinGPT6ModelGroups)+len(c.ModelGroups))
	for name, members := range builtinGPT6ModelGroups {
		rawGroups[name] = modelGroupConfig{Members: members}
	}
	configuredNames := make(map[string]struct{}, len(c.ModelGroups))
	for name, group := range c.ModelGroups {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		configuredNames[key] = struct{}{}
		rawGroups[key] = group
	}

	rawNames := make([]string, 0, len(rawGroups))
	for rawName := range rawGroups {
		rawNames = append(rawNames, rawName)
	}
	sort.Strings(rawNames)

	out := make(map[string]core.ModelGroup, len(rawGroups))
	for _, rawName := range rawNames {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if name == "" {
			continue
		}
		if _, exists := out[name]; exists {
			continue
		}

		raw := rawGroups[rawName]
		_, configured := configuredNames[name]
		group := core.ModelGroup{Builtin: !configured}
		seenMembers := make(map[string]struct{}, len(raw.Members))
		for _, rawMember := range raw.Members {
			client, model, ok := strings.Cut(strings.TrimSpace(rawMember), "/")
			client = strings.TrimSpace(client)
			model = strings.TrimSpace(model)
			if !ok || client == "" || model == "" {
				continue
			}
			key := strings.ToLower(client) + "\x00" + strings.ToLower(model)
			if _, duplicate := seenMembers[key]; duplicate {
				continue
			}
			seenMembers[key] = struct{}{}
			group.Members = append(group.Members, core.ModelGroupMember{Client: client, Model: model})
		}
		if len(group.Members) == 0 {
			continue
		}

		if len(raw.PlatformPriorities) > 0 {
			group.PlatformPriorities = make(map[string]int, len(raw.PlatformPriorities))
			for platform, priority := range raw.PlatformPriorities {
				platform = strings.TrimSpace(platform)
				if platform != "" {
					group.PlatformPriorities[platform] = priority
				}
			}
		}
		out[name] = group
	}
	return out
}

// scheduleConfig drives the background scheduler.  The hour lists are local
// (Asia/Shanghai) hours at which the corresponding batch runs; a list that is
// empty disables that batch even when the master switch is on.
type scheduleConfig struct {
	Enabled bool `json:"enabled"`

	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	BlackcatHours  []int `json:"blackcat_hours"`
	GrowthHours    []int `json:"growth_hours"`

	CheckinEnabled   *bool `json:"checkin_enabled"`
	TravelEnabled    *bool `json:"travel_enabled"`
	ActivityEnabled  *bool `json:"activity_enabled"`
	KeepaliveEnabled *bool `json:"keepalive_enabled"`
	BlackcatEnabled  *bool `json:"blackcat_enabled"`
	GrowthEnabled    *bool `json:"growth_enabled"`

	BalanceRefreshEnabled *bool `json:"balance_refresh_enabled"`
	BalanceRefreshMinutes int   `json:"balance_refresh_minutes"`

	RecoveryEnabled       *bool `json:"recovery_enabled"`
	RecoveryEveryMinutes  *int  `json:"recovery_every_minutes"`
	RecoveryJitterMinutes *int  `json:"recovery_jitter_minutes"`

	// DailyBalance is the shared early-morning balance sweep.  A nil hour
	// list means "the default midnight"; an explicit empty list means
	// "never run it here".
	DailyBalanceEnabled *bool `json:"daily_balance_enabled"`
	DailyBalanceHours   []int `json:"daily_balance_hours"`

	// Clients overrides the shared timetable for one platform.  The outer
	// key is a client name, the inner key a batch name ("checkin", "travel",
	// ...); both are matched case-insensitively.  A (client, batch) pair
	// that is absent here follows the group above, which is what makes an
	// empty clients block the reference behaviour.
	Clients map[string]map[string]scheduleOverride `json:"clients"`
}

// scheduleOverride is one platform's private timetable for one batch.
// Hours is a slice rather than a pointer so an explicit [] (never run this
// one here) stays distinguishable from an absent key (no override at all).
type scheduleOverride struct {
	Enabled       *bool                 `json:"enabled"`
	Hours         []int                 `json:"hours"`
	EveryMinutes  *int                  `json:"every_minutes"`
	JitterMinutes *int                  `json:"jitter_minutes"`
	Accounts      *scheduleAccountScope `json:"accounts,omitempty"`
}

type scheduleAccountScope struct {
	Mode    string   `json:"mode"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// schedule projects the file's timetable onto the scheduler's config.  A group
// needs both its switch and at least one hour: an empty hour list is how an
// operator says "never run this one", and the scheduler rejects an enabled
// group that lists no hours.
func (c *fileConfig) schedule() scheduler.Config {
	s := c.Schedule
	cfg := scheduler.Config{
		Enabled:      s.Enabled,
		Checkin:      schedGroup(s.CheckinEnabled, s.CheckinHours),
		Travel:       schedGroup(s.TravelEnabled, s.TravelHours),
		Activity:     schedGroup(s.ActivityEnabled, s.ActivityHours),
		Keepalive:    schedGroup(s.KeepaliveEnabled, s.KeepaliveHours),
		Blackcat:     schedGroup(s.BlackcatEnabled, s.BlackcatHours),
		Growth:       schedGroup(s.GrowthEnabled, s.GrowthHours),
		Recovery:     s.recoveryGroup(),
		DailyBalance: s.dailyBalanceGroup(),
	}
	// The master switch is the real gate, so every group defaults to on — and
	// so does the balance tick, matching the reference, whose README documents
	// the refresh as "缺省开启".  Keeping credits fresh between check-ins is
	// what lets a balance-recovered account unfreeze by itself, so an install
	// that quietly lost that would behave differently from the original.  The
	// tick still needs a positive interval: 0 turns it off without having to
	// touch the switch.
	cfg.BalanceRefresh.Enabled = boolOr(s.BalanceRefreshEnabled, true) && s.BalanceRefreshMinutes > 0
	cfg.BalanceRefresh.Every = time.Duration(s.BalanceRefreshMinutes) * time.Minute
	cfg.Clients = s.schedClients()
	return cfg
}

func schedGroup(enabled *bool, hours []int) scheduler.Group {
	return scheduler.Group{Enabled: boolOr(enabled, true) && len(hours) > 0, Hours: hours}
}

const (
	defaultRecoveryEvery  = 4 * time.Hour
	defaultRecoveryJitter = time.Hour
)

// defaultDailyBalanceHour is when the shared daily balance sweep starts.  The
// scheduler spreads the actual fire across the following window.
const defaultDailyBalanceHour = 0

// recoveryGroup projects the shared periodic recovery task.  The interval and
// jitter use pointers so an explicitly configured zero jitter stays zero
// rather than being replaced by the default.
func (s scheduleConfig) recoveryGroup() scheduler.Group {
	everyMinutes := intOr(s.RecoveryEveryMinutes, int(defaultRecoveryEvery/time.Minute))
	jitterMinutes := intOr(s.RecoveryJitterMinutes, int(defaultRecoveryJitter/time.Minute))
	return scheduler.Group{
		Enabled: boolOr(s.RecoveryEnabled, true),
		Every:   time.Duration(everyMinutes) * time.Minute,
		Jitter:  time.Duration(jitterMinutes) * time.Minute,
	}
}

// dailyBalanceGroup projects the shared daily balance sweep.  A nil hour list
// falls back to the default midnight; an explicit empty list stays empty, which
// schedGroup turns into "disabled".  The distinction matters because an
// operator who clears the hours box expects the task to stop, not to snap back
// to midnight.
func (s scheduleConfig) dailyBalanceGroup() scheduler.Group {
	hours := s.DailyBalanceHours
	if hours == nil {
		hours = []int{defaultDailyBalanceHour}
	}
	return schedGroup(s.DailyBalanceEnabled, hours)
}

// schedClients projects the per-platform overrides onto the scheduler's own
// config.  A pair with neither an explicit switch nor an explicit hours list
// is not an override at all, so it is dropped instead of being materialised
// as a group nobody asked for.
func (s scheduleConfig) schedClients() map[string]map[string]scheduler.Group {
	if len(s.Clients) == 0 {
		return nil
	}
	sharedRecovery := s.recoveryGroup()
	sharedDaily := s.dailyBalanceGroup()
	out := make(map[string]map[string]scheduler.Group, len(s.Clients))
	for client, byBatch := range s.Clients {
		for batch, ov := range byBatch {
			isRecovery := strings.EqualFold(strings.TrimSpace(batch), scheduler.RecoveryTaskName)
			isDaily := strings.EqualFold(strings.TrimSpace(batch), scheduler.DailyBalanceTaskName)
			if isRecovery {
				if ov.Enabled == nil && ov.EveryMinutes == nil && ov.JitterMinutes == nil && ov.Accounts == nil {
					continue
				}
			} else if ov.Enabled == nil && ov.Hours == nil && ov.Accounts == nil {
				continue
			}
			if out[client] == nil {
				out[client] = make(map[string]scheduler.Group, len(byBatch))
			}
			group := schedGroup(ov.Enabled, ov.Hours)
			if isRecovery {
				group = sharedRecovery
				if ov.Enabled != nil {
					group.Enabled = *ov.Enabled
				}
				if ov.EveryMinutes != nil {
					group.Every = time.Duration(*ov.EveryMinutes) * time.Minute
				}
				if ov.JitterMinutes != nil {
					group.Jitter = time.Duration(*ov.JitterMinutes) * time.Minute
				}
			} else if isDaily {
				// The daily sweep is hour-based like a batch, so a partial override
				// inherits the shared switch and replaces only the hours it names.
				group = sharedDaily
				if ov.Hours != nil {
					group.Hours = ov.Hours
					group.Enabled = boolOr(ov.Enabled, true) && len(ov.Hours) > 0
				} else if ov.Enabled != nil {
					group.Enabled = *ov.Enabled
				}
			}
			group.Accounts = schedAccountScope(ov.Accounts)
			out[client][batch] = group
		}
	}
	return out
}

func schedAccountScope(s *scheduleAccountScope) scheduler.AccountScope {
	if s == nil {
		return scheduler.AccountScope{}
	}
	return scheduler.AccountScope{
		Mode:    strings.TrimSpace(s.Mode),
		Include: append([]string(nil), s.Include...),
		Exclude: append([]string(nil), s.Exclude...),
	}
}

type promptConfig struct {
	Mode string `json:"mode"`
	File string `json:"file"`
}

type sessionStickyConfig struct {
	Enabled    *bool  `json:"enabled"`
	TTL        string `json:"ttl"`
	GCInterval string `json:"gc_interval"`
}

// The reference's documented pool ceilings: 3 concurrent upstream requests per
// account, and 2 on the realm whose risk control is stricter.  They are applied
// only when the file does not mention the key, because 0 is a meaningful value
// for max_in_flight ("no ceiling") and a default must not overwrite it.
const (
	defaultMaxInFlight       = 3
	defaultMaxInFlightGlobal = 2
	// The platform-level brakes are absent-by-default in the file, so the
	// projection applies these when the key is missing.  An explicit 0 still
	// means "no ceiling" and is preserved by intOr.
	defaultPlatformMaxInFlight           = 2
	defaultPlatformMaxInFlightPerAccount = 2
	// defaultPackageDetailLimit is how many credit batches the credits view
	// shows per account before collapsing the rest.  It is the reference's
	// panel.package_detail_limit default.
	defaultPackageDetailLimit = 5
)

// poolConfig mirrors the reference's account-pool tuning.  Durations are
// strings ("30m") so an operator can read the file without a comment.
type poolConfig struct {
	BreakerThreshold    int     `json:"breaker_threshold"`
	BreakerCooldown     string  `json:"breaker_cooldown"`
	BreakerCooldownMax  string  `json:"breaker_cooldown_max"`
	DegradeThreshold    int     `json:"degrade_threshold"`
	DegradeCooldown     string  `json:"degrade_cooldown"`
	DegradeCooldownMax  string  `json:"degrade_cooldown_max"`
	MaxInFlight         *int    `json:"max_in_flight"`
	MaxInFlightGlobal   *int    `json:"max_in_flight_global"`
	IdleWeightPerHour   float64 `json:"idle_weight_per_hour"`
	IdleWeightMax       float64 `json:"idle_weight_max"`
	PreferExpiring      *bool   `json:"prefer_expiring"`
	ExpiringSoon        string  `json:"expiring_soon"`
	CostExploreInterval string  `json:"cost_explore_interval"`
}

type cooldownConfig struct {
	SoftRate    string `json:"soft_rate"`
	SoftRateMax string `json:"soft_rate_max"`
}

type featuresConfig struct {
	SanitizeBlacklistFingerprints *bool `json:"sanitize_blacklist_fingerprints"`
}

// panelConfig holds the dashboard's own presentation settings -- the ones the
// operator tunes because a page is too dense, not because a client behaves
// differently.  They are part of the shared config file rather than a client's
// block because the panel is shared: one value drives every module's view.
type panelConfig struct {
	// PackageDetailLimit is how many credit batches the credits view shows per
	// account before collapsing the rest behind a toggle.  The batches arrive
	// sorted by expiry, so the visible ones are the ones about to lapse.  A
	// non-positive value means "unset" and selects the documented default;
	// there is no "show everything" spelling because the page already offers a
	// per-group expand, and an account with hundreds of expired batches would
	// otherwise render hundreds of rows on every refresh.
	PackageDetailLimit int `json:"package_detail_limit"`
}

// applyDefaults fills every unset section with the reference's documented
// default.  It is a method (not a constructor) so that Reload can reuse it on
// a freshly decoded file.
func (c *fileConfig) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8788"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	if c.Schedule.BalanceRefreshMinutes <= 0 {
		c.Schedule.BalanceRefreshMinutes = 5
	}
	if c.Schedule.RecoveryEnabled == nil {
		c.Schedule.RecoveryEnabled = boolPtr(true)
	}
	if c.Schedule.RecoveryEveryMinutes == nil {
		c.Schedule.RecoveryEveryMinutes = ptrInt(int(defaultRecoveryEvery / time.Minute))
	}
	if c.Schedule.RecoveryJitterMinutes == nil {
		c.Schedule.RecoveryJitterMinutes = ptrInt(int(defaultRecoveryJitter / time.Minute))
	}
	if c.Prompt.Mode == "" {
		c.Prompt.Mode = "passthrough"
	}
	if c.SessionSticky.TTL == "" {
		c.SessionSticky.TTL = "30m"
	}
	if c.SessionSticky.GCInterval == "" {
		c.SessionSticky.GCInterval = "5m"
	}
	if c.SessionSticky.Enabled == nil {
		// session_sticky.enabled defaults to true when the file is silent, the
		// same rule the reference applies (cmd/server/config.go:234).  It is a
		// pointer because "absent" and "explicitly false" must stay
		// distinguishable: only the latter turns stickiness off.
		c.SessionSticky.Enabled = boolPtr(true)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.BreakerCooldown == "" {
		c.Pool.BreakerCooldown = "30m"
	}
	if c.Pool.BreakerCooldownMax == "" {
		c.Pool.BreakerCooldownMax = "6h"
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	if c.Pool.MaxInFlight == nil {
		v := defaultMaxInFlight
		c.Pool.MaxInFlight = &v
	}
	// The global tier's zero means "not set", so an unset or non-positive value
	// falls back to the default: unlike max_in_flight, 0 here cannot mean "no
	// ceiling", or the stricter realm would silently lose its ceiling.
	if c.Pool.MaxInFlightGlobal == nil || *c.Pool.MaxInFlightGlobal <= 0 {
		v := defaultMaxInFlightGlobal
		c.Pool.MaxInFlightGlobal = &v
	}
	if c.Pool.IdleWeightPerHour == 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax == 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Pool.ExpiringSoon == "" {
		// The reference defaults this to 168h.  It is the window inside which
		// credit counts as "about to lapse", and a window of an hour made
		// almost no credit qualify, so the earliest-expiry router never had
		// anything to prefer.
		c.Pool.ExpiringSoon = "168h"
	}
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.Cooldown.SoftRate == "" {
		c.Cooldown.SoftRate = "600s"
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.Panel.PackageDetailLimit <= 0 {
		// The reference's normalize() does the same, so an operator who writes
		// 0 or a negative number gets the default rather than an empty table.
		c.Panel.PackageDetailLimit = defaultPackageDetailLimit
	}
}

// ---------------------------------------------------------------------------
// environment overrides
// ---------------------------------------------------------------------------

// The reference is configured with WB2A_* environment variables, and this
// program's own knobs are named CLIENT2API_*.  Every knob accepts both
// spellings: an operator migrating from workbuddy2api-panel keeps the
// environment they already deploy, and a new deployment gets a prefix that
// names this program.  The specific spelling wins when both are set.
const (
	envPrefix = "CLIENT2API_"
	refPrefix = "WB2A_"
)

// envClient is the per-client block holding upstream identity -- user agent,
// client versions, device token, timeouts.  The reference kept these in one
// global upstream.* section because it served a single vendor; here they belong
// to a client, so the WB2A_* spellings land in workbuddy's block, which is the
// client those variables were written for.
const envClient = "workbuddy"

// envValue returns the first non-empty value among names.
func envValue(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// envNames expands a knob's short name into its two spellings, this program's
// first.
func envNames(short string) []string {
	return []string{envPrefix + short, refPrefix + short}
}

// envBool parses a boolean knob.  ok is false when nothing was set or when the
// value did not parse: the reference ignores an unparseable value and keeps the
// file's, and a typo that silently flipped a safety switch would be worse than
// the typo itself.
func envBool(short string) (value, ok bool) {
	v := envValue(envNames(short)...)
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

// envInt is envBool for integer knobs, under the same ignore-a-typo rule.
func envInt(short string) (value int, ok bool) {
	v := envValue(envNames(short)...)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// setClientKey writes one key into a client's configuration block.
//
// The blocks are json.RawMessage because the shared layer must not know a
// module's schema, so an override has to re-encode the block rather than assign
// a field.  A block that is present but not a JSON object is left untouched:
// replacing an operator's value with a fragment of itself would be worse than
// ignoring the override, and the module will complain about the block in its
// own words.
func (c *fileConfig) setClientKey(client, key string, value any) {
	block := map[string]any{}
	if raw, ok := c.Clients[client]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &block); err != nil {
			return
		}
		if block == nil {
			// A literal null decodes to a nil map; assigning into it would
			// panic, and the operator plainly meant "no settings here".
			block = map[string]any{}
		}
	}
	block[key] = value
	b, err := json.Marshal(block)
	if err != nil {
		return
	}
	if c.Clients == nil {
		c.Clients = make(map[string]json.RawMessage, 1)
	}
	c.Clients[client] = b
}

// applyEnv overlays the environment onto the decoded file.
//
// It runs after the file is read and before applyDefaults, which is the
// reference's order (Load: file -> applyEnv -> normalize) and the only order in
// which "set in the environment, absent from the file" can win: a defaulting
// pass that ran first would fill the knob in and hide the fact that it was
// unset, and one that ran later would overwrite what the environment just said.
// Precedence is therefore defaults < file < environment.
//
// Only a non-empty value overrides, so "unset" and "set to empty" mean the same
// thing and a container that inherits a blank variable cannot blank a setting.
// A value that does not parse is ignored for the same reason.
//
// AUTH_DIR and STATE_FILE are the reference's two path knobs.  Credentials live
// under one directory here, so AUTH_DIR maps to workbuddy's accounts_dir and
// STATE_FILE has no counterpart: the pool's runtime state sits beside the
// credentials it describes and is not separately addressable.
func (c *fileConfig) applyEnv() {
	if v := envValue(envNames("LISTEN")...); v != "" {
		c.Listen = v
	}
	if v := envValue(envNames("API_KEY")...); v != "" {
		c.APIKey = v
	}
	if v := envValue(envNames("DATA_DIR")...); v != "" {
		c.DataDir = v
	}
	if v := envValue(envNames("PROXY")...); v != "" {
		c.Proxy = v
	}
	if v := envValue(envNames("SOFT_RATE")...); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := envValue(envNames("SOFT_RATE_MAX")...); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := envValue(envNames("PROMPT_MODE")...); v != "" {
		c.Prompt.Mode = v
	}
	if v := envValue(envNames("PROMPT_FILE")...); v != "" {
		c.Prompt.File = v
	}
	if v := envValue(envNames("EXPIRING_SOON")...); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if b, ok := envBool("PREFER_EXPIRING"); ok {
		c.Pool.PreferExpiring = &b
	}
	if b, ok := envBool("SANITIZE_FINGERPRINTS"); ok {
		c.Features.SanitizeBlacklistFingerprints = &b
	}

	// The upstream identity knobs.  These are strings, so an empty value is
	// simply "not set" -- there is no way to clear one from the environment,
	// which is the reference's behaviour too.
	for _, kv := range []struct{ short, key string }{
		{"AUTH_DIR", "accounts_dir"},
		{"USER_AGENT", "user_agent"},
		{"CLIENT_VERSION", "client_version"},
		{"CLI_VERSION", "cli_version"},
		{"CLIENT_NAME", "client_name"},
		{"DEVICE_TOKEN", "device_token"},
		{"DEVICE_TOKEN_FILE", "device_token_file"},
	} {
		if v := envValue(envNames(kv.short)...); v != "" {
			c.setClientKey(envClient, kv.key, v)
		}
	}
	for _, kv := range []struct{ short, key string }{
		{"TIMEOUT_SECONDS", "timeout_seconds"},
		{"HEADER_TIMEOUT_SECONDS", "header_timeout_seconds"},
		{"IDLE_TIMEOUT_SECONDS", "idle_timeout_seconds"},
	} {
		if n, ok := envInt(kv.short); ok {
			c.setClientKey(envClient, kv.key, n)
		}
	}
	if b, ok := envBool("PASSTHROUGH_IP"); ok {
		c.setClientKey(envClient, "passthrough_ip", b)
	}
}

// boolOr resolves an optional switch against a default.
func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// boolPtr boxes a literal so it can be stored in an optional switch field.
func boolPtr(v bool) *bool { return &v }

// intOr resolves an optional number against a default.  It only exists for the
// log line and the tests: applyDefaults has already filled the real value in by
// the time anyone reads the config.
func intOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// dur parses a configuration duration, falling back to def on anything
// unparseable so a typo cannot make the process refuse to start.
func dur(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// durAllowZero is dur for the one knob where zero is a real setting rather than
// a missing value: cost_explore_interval's "0" turns the exploration off, which
// an operator must be able to say.  Anything unparseable still falls back to
// def, so a typo cannot silently disable a feature.
func durAllowZero(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// ptrInt, ptrDur and ptrF64 hand a copy of a configured value to the modules as
// a pointer, which is how they tell "the file said so" apart from "the file was
// silent".  The config has already been defaulted by the time these run, so the
// value is always the operator's own or the documented default.
func ptrInt(v int) *int                     { return &v }
func ptrDur(v time.Duration) *time.Duration { return &v }
func ptrF64(v float64) *float64             { return &v }

// poolTuning projects the pool.* block onto the module-facing policy.  Every
// field is optional, so a reload that only touches one knob leaves the rest of
// the pool exactly as it was.
func (c *fileConfig) poolTuning() *core.PoolTuning {
	return &core.PoolTuning{
		BreakerThreshold:    ptrInt(c.Pool.BreakerThreshold),
		BreakerCooldown:     ptrDur(dur(c.Pool.BreakerCooldown, 30*time.Minute)),
		BreakerCooldownMax:  ptrDur(dur(c.Pool.BreakerCooldownMax, 6*time.Hour)),
		DegradeThreshold:    ptrInt(c.Pool.DegradeThreshold),
		DegradeCooldown:     ptrDur(dur(c.Pool.DegradeCooldown, 10*time.Minute)),
		DegradeCooldownMax:  ptrDur(dur(c.Pool.DegradeCooldownMax, 2*time.Hour)),
		SoftRateMax:         ptrDur(dur(c.Cooldown.SoftRateMax, 2*time.Hour)),
		IdleWeightPerHour:   ptrF64(c.Pool.IdleWeightPerHour),
		IdleWeightMax:       ptrF64(c.Pool.IdleWeightMax),
		PreferExpiring:      c.Pool.PreferExpiring,
		CostExploreInterval: ptrDur(durAllowZero(c.Pool.CostExploreInterval, 30*time.Minute)),
		ExpiringSoon:        ptrDur(dur(c.Pool.ExpiringSoon, 168*time.Hour)),
	}
}

// refreshBalances asks every module that can report a balance for one, account
// by account.
//
// This is what lets a credit park lift by itself: the module feeds the number
// into its pool, and the pool unfreezes an account the moment the vendor grants
// more.  Without a sweep of its own the park would only ever lift when an
// operator happened to open the dashboard and the panel asked for the same
// number on their behalf.
//
// Capability-gated on both halves: a module that cannot report balances, or
// cannot list its accounts, is skipped rather than guessed at.
func refreshBalances(ctx context.Context, reg *core.Registry, soon time.Duration, logger *log.Logger) {
	refreshBalancesMatching(ctx, reg, "", soon, logger, false, false)
}

// refreshBalancesForClient is the scheduled recovery sweep.  It is scoped to
// one platform, includes accounts parked in recoverable states, and bypasses
// the module's quiet-probe gate because the scheduler already paced the probe.
func refreshBalancesForClient(ctx context.Context, reg *core.Registry, client string, soon time.Duration, logger *log.Logger, includeRecoverable bool) {
	refreshBalancesMatching(ctx, reg, client, soon, logger, includeRecoverable, includeRecoverable)
}

func refreshBalancesMatching(ctx context.Context, reg *core.Registry, client string, soon time.Duration, logger *log.Logger, includeRecoverable, ignoreGate bool) {
	client = strings.TrimSpace(client)
	for _, c := range reg.All() {
		if client != "" && !strings.EqualFold(c.Name(), client) {
			continue
		}
		bp, ok := core.AsBalanceProvider(c)
		if !ok {
			continue
		}
		am, ok := core.AsAccountManager(c)
		if !ok {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		accounts, err := am.Accounts(lctx)
		cancel()
		if err != nil {
			logf(logger, "[%s] balance sweep: %v", c.Name(), err)
			continue
		}
		for _, acct := range accounts {
			if !balanceProbeEligible(acct.State, includeRecoverable) {
				continue
			}
			if !ignoreGate && !core.BackgroundProbeAllowed(ctx, c, acct.ID) {
				continue
			}
			// One dead credential must not abort the sweep.  The pool hears
			// about that failure through the chat path, which is where a
			// failure can actually be classified; here we only need the
			// accounts that still answer.
			bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := bp.AccountBalance(bctx, acct.ID, soon)
			cancel()
			if err != nil {
				logf(logger, "[%s] balance sweep %s: %v", c.Name(), core.MaskSecret(acct.ID), err)
			}
		}
	}
}

// balanceProbeEligible separates the global soft sweep from the scheduled
// recovery sweep.  The soft sweep leaves self-parking states alone; recovery
// includes those temporary verdicts but still skips terminal credential errors.
func balanceProbeEligible(state string, includeRecoverable bool) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	if includeRecoverable {
		switch state {
		case "", "ready", "unknown":
			return true
		}
		return core.IsRecoverableAccountState(state)
	}
	return !core.IsRecoverableAccountState(state)
}

// liveSnapshot projects the file config onto the hot-editable snapshot.
func (c *fileConfig) liveSnapshot() livecfg.Snapshot {
	return livecfg.Snapshot{
		APIKey:               c.APIKey,
		SoftCooldown:         dur(c.Cooldown.SoftRate, 600*time.Second),
		SanitizeFingerprints: boolOr(c.Features.SanitizeBlacklistFingerprints, true),
		PromptMode:           c.Prompt.Mode,
		PromptFile:           c.Prompt.File,
		AffinityTTL:          dur(c.SessionSticky.TTL, core.DefaultAffinityTTL),
		ExpiringSoon:         dur(c.Pool.ExpiringSoon, 168*time.Hour),
	}
}

// liveSettings is the module-facing half of the same projection.  A nil
// pointer deliberately means "the file said nothing", so a partial reload
// never clears a value the operator did not touch.
func (c *fileConfig) liveSettings() core.LiveSettings {
	return core.LiveSettings{
		SanitizeFingerprints: c.Features.SanitizeBlacklistFingerprints,
		PromptMode:           c.Prompt.Mode,
		PromptFile:           c.Prompt.File,
		MaxInFlight:          c.Pool.MaxInFlight,
		MaxInFlightGlobal:    c.Pool.MaxInFlightGlobal,
		// session_sticky.* used to be parsed, defaulted and logged without
		// anyone reading it.  It now reaches the tables that actually pin a
		// conversation, so an operator who tunes the window -- or turns the
		// feature off entirely -- sees it take effect on the next reload.
		AffinityEnabled:    c.SessionSticky.Enabled,
		AffinityTTL:        dur(c.SessionSticky.TTL, core.DefaultAffinityTTL),
		AffinityGCInterval: dur(c.SessionSticky.GCInterval, core.DefaultAffinityGCInterval),
		// pool.breaker_*, pool.degrade_*, pool.soft_rate_max,
		// pool.idle_weight_*, pool.prefer_expiring and
		// pool.cost_explore_interval were parsed, defaulted and logged without
		// a single reader.  They now reach the pool that acts on them.
		Pool: c.poolTuning(),
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "client2api:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", filepath.Join("configs", "client2api.json"), "path to the JSON config file")
		listenFlag = flag.String("listen", "", "override listen address")
		dataDir    = flag.String("data-dir", "", "override data directory")
		proxyFlag  = flag.String("proxy", "", "override outbound HTTP proxy, e.g. http://127.0.0.1:8080")
		listOnly   = flag.Bool("list-clients", false, "print compiled-in clients and exit")
		trayFlag   = flag.Bool("tray", true, "show a notification-area icon while the gateway runs (Windows only)")
	)
	flag.Parse()

	if *listOnly {
		fmt.Println(strings.Join(core.Registered(), "\n"))
		return nil
	}

	// A -config the operator typed is a contract, not a hint.  Creating the
	// file when it is missing is the right answer for the default path (a
	// fresh install should just start), but it is the wrong answer for a path
	// someone wrote out by hand: a typo then produces a second installation in
	// whatever directory the process happened to start in, with its own
	// generated api_key and its own data tree, while the real deployment sits
	// untouched and apparently ignored.
	explicitConfig := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	cfg, created, err := loadConfigAt(*configPath, version, !explicitConfig)
	if err != nil {
		if explicitConfig && errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("-config %s does not exist; refusing to start a second installation here (drop the flag to create a fresh config in the working directory)", *configPath)
		}
		return err
	}
	if *listenFlag != "" {
		cfg.Listen = *listenFlag
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *proxyFlag != "" {
		cfg.Proxy = *proxyFlag
	}
	cfg.applyDefaults()
	if err := core.EnsureDir(cfg.DataDir); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}

	// The gateway's instrumentation is created here, before the logger, so the
	// log ring can be one of the logger's sinks.  The same values are handed
	// to the gateway (which writes them) and the panel (which renders them),
	// so the two can never disagree.
	stats := gateway.NewStats()
	// Usage survives a restart: the reference keeps its rollup in data/, and an
	// operator reading /panel/api/usage after a deploy should see history, not a
	// blank page.  The store flushes on a timer and again on shutdown.
	usage := gateway.NewPersistentUsageStore(gateway.DefaultUsageRecords,
		filepath.Join(cfg.DataDir, gateway.DefaultUsageFileName))
	logs := gateway.NewLogRing(gateway.DefaultLogCapacity)

	// Keep a durable log beside the data.  The packaged executable is built
	// as a windowsgui process, so stdout may be an invalid handle and the file
	// is the only useful record after the tray process has started.
	logPath := filepath.Join(cfg.DataDir, "logs", "client2api.log")
	logFile, logErr := logfile.Open(logPath, logfile.DefaultMaxBytes, logfile.DefaultBackups)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "client2api: could not open %s: %v\n", logPath, logErr)
	} else {
		defer logFile.Close()
	}

	// Everything the process logs goes to the panel ring, the durable file and
	// stdout.  Fanout attempts every sink, so an invalid windowsgui stdout
	// handle cannot swallow the file and ring copies.
	logSink := logfile.Fanout(logs, logFile, os.Stdout)
	logger := log.New(logSink, "", log.LstdFlags|log.Lmsgprefix)
	// The per-request rows are not log records -- they are written on a bare
	// Fprintf so that they never carry a timestamp prefix and never fight the
	// logger for the ring's lock.
	gateway.SetChatLogOutput(logSink)

	// Platform-health alerts are durable and low-frequency.  The tray pointer
	// is filled after its message loop is ready; requests may start slightly
	// earlier, so the closure reads it under a mutex rather than capturing a
	// nil icon forever.
	alertStore := alerts.NewStore(alerts.DefaultLimit, filepath.Join(cfg.DataDir, "alerts.json"))
	if err := alertStore.Load(); err != nil {
		logger.Printf("alerts: could not load history: %v", err)
	}
	var trayIconMu sync.Mutex
	var trayIcon *tray.Icon
	notifyAlert := func(a alerts.Alert) {
		alertStore.Add(a)
		trayIconMu.Lock()
		icon := trayIcon
		trayIconMu.Unlock()
		if icon == nil {
			return
		}
		icon.Notify("client2api 平台告警", fmt.Sprintf("%s / %s 连续失败，已临时降级 10 分钟", a.Client, a.Model))
	}
	// Usage persistence is best-effort by design: a read-only data dir must not
	// stop the gateway, so both failures are reported and survived.
	usage.SetLogger(func(msg string, err error) {
		logger.Printf("%s: %v", msg, err)
	})
	if err := usage.Load(); err != nil {
		logger.Printf("usage: could not load history: %v", err)
	}
	usage.Start()
	httpClient, err := newHTTPClient(cfg.Proxy)
	if err != nil {
		return err
	}
	if created {
		logger.Printf("wrote a fresh config to %s with a generated api_key", *configPath)
	} else if cfg.APIKey == "" {
		logger.Printf("warning: no api_key is configured, so the gateway and the panel are UNAUTHENTICATED; set one in the config page or in %s", *configPath)
	}

	live := livecfg.New(cfg.liveSnapshot())
	guard := core.NewGuard(logger.Printf)

	disabled := map[string]bool{}
	for _, n := range cfg.Disabled {
		disabled[strings.TrimSpace(n)] = true
	}

	registry := core.NewRegistry()
	loaded, skipped := 0, 0
	for _, name := range core.Registered() {
		if disabled[name] {
			logger.Printf("[%s] disabled by config, skipping", name)
			skipped++
			continue
		}
		modDir := filepath.Join(cfg.DataDir, name)
		if err := core.EnsureDir(modDir); err != nil {
			return fmt.Errorf("data dir for %s: %w", name, err)
		}
		deps := core.Deps{
			DataDir:    modDir,
			Config:     cfg.Clients[name],
			HTTPClient: httpClient,
			Proxy:      cfg.Proxy,
			Guard:      guard,
			Logf:       func(format string, args ...any) { logger.Printf("["+name+"] "+format, args...) },
		}
		client, err := core.Build(name, deps)
		if err != nil {
			// One broken module must never take the process down: that is the
			// whole point of the isolation rule.
			logger.Printf("[%s] FAILED to load: %v", name, err)
			skipped++
			continue
		}
		registry.Add(client)
		loaded++
		logger.Printf("[%s] loaded", name)
	}
	sourceDeps := core.Deps{DataDir: cfg.DataDir, HTTPClient: httpClient, Proxy: cfg.Proxy, Guard: guard,
		Logf: func(format string, args ...any) { logger.Printf("[sources] "+format, args...) }}
	if err := registry.ReconcileSources(cfg.Sources, sourceDeps); err != nil {
		return err
	}
	for alias, target := range cfg.Aliases {
		registry.AddAlias(alias, target)
		logger.Printf("alias %s -> %s", alias, target)
	}
	// The per-platform routing policy is installed before the server starts
	// accepting, so the very first request already honours priority and the
	// blacklist.  An absent platforms block installs the default everywhere.
	platCfgs := cfg.platformConfigsFor(registry.All())
	registry.SetPlatformConfigs(platCfgs)
	registry.SetModelGroups(cfg.modelGroups())
	// A module that owns an account pool reads its own policy from the push, so
	// the low-balance guard is armed before the first request too.
	if n := core.ApplyPlatformPolicies(registry, platCfgs); n > 0 {
		logger.Printf("platform policy applied to %d module(s)", n)
	}
	if n := len(cfg.Platforms); n > 0 {
		logger.Printf("platform routing: %d platform(s) configured", n)
	}
	logger.Printf("clients: %d loaded, %d skipped, registered=%d", loaded, skipped, len(core.Registered()))
	if n := core.ApplyLive(registry, cfg.liveSettings()); n > 0 {
		logger.Printf("live settings applied to %d module(s)", n)
	}

	// The scheduler is vendor-blind: it learns what to run from each module's
	// own BatchPlan and never names a task code itself.  A module that plans
	// nothing is simply never scheduled.
	//
	// platformSweep is the per-platform recovery/daily work: refresh every
	// account's balance -- including the parked ones a fresh credit grant could
	// free -- and renew the idle credentials whose expiry crept inside the
	// module's refresh margin.  The daily sweep and the periodic recovery probe
	// are the same work at different cadences, so they share one closure.
	platformSweep := func(ctx context.Context, client string) {
		soon := dur(cfg.Pool.ExpiringSoon, 168*time.Hour)
		if d := live.Load().ExpiringSoon; d > 0 {
			soon = d
		}
		refreshBalancesForClient(ctx, registry, client, soon, logger, true)
		refreshExpiringAccountsForClient(ctx, registry, client, time.Now(), logger)
	}

	sched := scheduler.New(scheduler.Deps{
		Registry: registry,
		Logf:     logger.Printf,
		// The finished-run journal sits beside the usage file, so the
		// tasks centre can still show yesterday's batches after a restart.
		HistoryPath: filepath.Join(cfg.DataDir, "schedule_runs.json"),
		OnBalanceRefresh: func(ctx context.Context) {
			// Two passes.  The first asks every module that can report a
			// balance for one, which is how a credit park lifts by itself: the
			// module feeds the number into its pool and the pool unfreezes the
			// account.  The second refreshes the model lists, where the vendors
			// that do expose credits put them.  Modules without a capability
			// are skipped by the type assertion.
			soon := dur(cfg.Pool.ExpiringSoon, 168*time.Hour)
			if d := live.Load().ExpiringSoon; d > 0 {
				soon = d
			}
			refreshBalances(ctx, registry, soon, logger)
			// An idle credential never reaches the chat path's proactive
			// refresh, so the same sweep window also renews the accounts whose
			// own expiry has crept inside their module's refresh margin.
			refreshExpiringAccounts(ctx, registry, time.Now(), logger)
			for _, c := range registry.All() {
				r, ok := c.(core.ModelRefresher)
				if !ok {
					continue
				}
				rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				_, err := r.RefreshModels(rctx)
				cancel()
				if err != nil {
					logger.Printf("[%s] balance refresh: %v", c.Name(), err)
				}
			}
		},
		OnRecoveryProbe:     platformSweep,
		OnDailyBalanceProbe: platformSweep,
	})
	sched.Reconfigure(cfg.schedule())
	logger.Printf("schedule: enabled=%v checkin=%v travel=%v activity=%v keepalive=%v blackcat=%v growth=%v",
		cfg.Schedule.Enabled, cfg.Schedule.CheckinHours, cfg.Schedule.TravelHours,
		cfg.Schedule.ActivityHours, cfg.Schedule.KeepaliveHours,
		cfg.Schedule.BlackcatHours, cfg.Schedule.GrowthHours)
	logger.Printf("schedule: daily_balance=%v recovery=%v", cfg.schedule().DailyBalance, cfg.schedule().Recovery)
	logger.Printf("prompt: mode=%s file=%q  session_sticky: enabled=%t ttl=%s gc=%s  pool: breaker=%d/%s idle_weight=%.2f in_flight=%d/%d",
		cfg.Prompt.Mode, cfg.Prompt.File, boolOr(cfg.SessionSticky.Enabled, true),
		cfg.SessionSticky.TTL, cfg.SessionSticky.GCInterval,
		cfg.Pool.BreakerThreshold, cfg.Pool.BreakerCooldown, cfg.Pool.IdleWeightPerHour,
		intOr(cfg.Pool.MaxInFlight, defaultMaxInFlight), intOr(cfg.Pool.MaxInFlightGlobal, defaultMaxInFlightGlobal))

	started := time.Now()
	// The path the process was actually started with, made absolute so the
	// panel reports something a human can act on.  Resolution failure is not
	// fatal: the panel then reports an unknown path instead of a wrong one.
	absConfigPath := ""
	if p, err := filepath.Abs(*configPath); err == nil {
		absConfigPath = p
	}

	// reload re-reads the config file and pushes the hot-editable subset at
	// both the gateway (through the live holder) and the modules (through the
	// optional LiveReloader capability).  Nothing is reconstructed, so an
	// in-flight request keeps the settings it started with.
	var reloadMu sync.Mutex
	reload := func() error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		// create=false: a reload must never invent a config.  With the file
		// deleted under a running process the old code minted a NEW api_key and
		// wrote it to disk, so a "reload" quietly changed the credential the
		// gateway accepts -- the exact opposite of reloading.
		next, _, err := loadConfigAt(absConfigPath, version, false)
		if err != nil {
			return err
		}
		next.applyDefaults()
		if err := registry.ReconcileSources(next.Sources, sourceDeps); err != nil {
			return err
		}
		live.Store(next.liveSnapshot())
		n := core.ApplyLive(registry, next.liveSettings())
		// Routing policy is hot too: a platform's priority or blacklist must
		// take effect on the next request, not only after a restart.
		nextPlat := next.platformConfigsFor(registry.All())
		registry.SetPlatformConfigs(nextPlat)
		core.ApplyPlatformPolicies(registry, nextPlat)
		// Aliases are hot too.  They used to be treated as a startup-only setting
		// and made the panel demand a restart for a one-line rename; Resolve
		// reads the table per request, so replacing it is enough.
		registry.SetAliases(next.Aliases)
		// Model groups are built from the same hot config and resolver table,
		// so adding or removing a member takes effect on the next request.
		registry.SetModelGroups(next.modelGroups())
		// The timetable is hot too: an operator who toggles a batch in the panel
		// expects the next fire to obey the new file, not the one from boot.
		sched.Reconfigure(next.schedule())
		logger.Printf("reload: live settings updated (api_key_set=%v, %d module(s) notified)",
			next.APIKey != "", n)
		return nil
	}

	// restart replaces this process with a fresh copy of itself, so a change the
	// live reload cannot apply (the listen address, the data directory, which
	// clients are enabled, a module's own settings) is one click in the panel
	// instead of "find a terminal and start it again".
	//
	// The replacement starts while this process still holds the port, so it is
	// marked as a hand-off and keeps retrying the bind until we exit and release
	// it.  Starting it first also means a replacement that dies during startup
	// does not take the running instance down with it: that is reported as an
	// error and the operator still has a working gateway.
	var restartMu sync.Mutex
	restarting := false
	restart := func() error {
		restartMu.Lock()
		defer restartMu.Unlock()
		if restarting {
			return errors.New("a restart is already in progress")
		}
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating this executable: %w", err)
		}
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("locating the working directory: %w", err)
		}
		// Drop any inherited marker before adding ours, so a process that is
		// restarted repeatedly does not accumulate copies of it.
		env := make([]string, 0, len(os.Environ())+1)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, restartHandoffEnv+"=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, restartHandoffEnv+"=1")

		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Dir = wd
		cmd.Env = env
		// Inherit the log destinations: the panel restart keeps writing to the
		// same place the process it replaced was writing to.
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.SysProcAttr = hiddenProcessAttr()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting the replacement process: %w", err)
		}

		// Watch it briefly.  The replacement blocks on the port we are still
		// holding, so it staying alive is the expected outcome; a process that
		// exits this fast failed on its own config and must not cost us the
		// running gateway.
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if err == nil {
				return errors.New("the replacement process exited during startup")
			}
			return fmt.Errorf("the replacement process exited during startup: %w", err)
		case <-time.After(1500 * time.Millisecond):
		}

		restarting = true
		go func() {
			// Give the HTTP response that asked for this time to reach the
			// browser before the listener goes away.
			time.Sleep(700 * time.Millisecond)
			logger.Printf("restart: handing over to a fresh process")
			os.Exit(0)
		}()
		return nil
	}

	modelOverrides, err := modelmeta.OpenOverrideStore(filepath.Join(cfg.DataDir, "model_context.json"), logger.Printf)
	if err != nil {
		logger.Printf("model metadata overrides unavailable: %v", err)
		modelOverrides, _ = modelmeta.OpenOverrideStore("")
	}

	panelHandler := panel.New(panel.Options{
		Registry:   registry,
		Version:    version,
		Listen:     cfg.Listen,
		Started:    started,
		Reload:     reload,
		Restart:    restart,
		ConfigPath: absConfigPath,
		// Where the modules keep their credential stores, so the panel can
		// export them and restore them into another instance.
		DataDir:        cfg.DataDir,
		AuthEnabled:    cfg.APIKey != "",
		Live:           live,
		Guard:          guard,
		Stats:          stats,
		Usage:          usage,
		Logs:           logs,
		Alerts:         alertStore,
		Scheduler:      sched,
		ModelOverrides: modelOverrides,
		// Same rule as the usage file: the probe results live in the data
		// directory, so the probe tool and the panel agree without a second
		// path setting.  Missing file = no annotation, never an error.
		ProbeFile: filepath.Join(cfg.DataDir, panel.DefaultProbeFileName),
		// The balance view asks a module how much of its credit lapses soon.
		// That window is pool.expiring_soon -- the reference threads its
		// scheduler's window in here, and this is where the operator actually
		// set it, so the dashboard and the pool agree on one number.
		ExpiringSoon: dur(cfg.Pool.ExpiringSoon, 168*time.Hour),
		// How many credit batches the credits view shows per account before
		// collapsing the rest.  applyDefaults has already resolved an unset
		// value to the reference default, so this is never zero here.
		PackageDetailLimit: cfg.Panel.PackageDetailLimit,
	})

	srv := gateway.NewServer(gateway.Options{
		Addr:           cfg.Listen,
		APIKey:         cfg.APIKey,
		Live:           live,
		Registry:       registry,
		Version:        version,
		Logger:         logger,
		Guard:          guard,
		Panel:          panelHandler,
		Stats:          stats,
		Usage:          usage,
		NotifyAlert:    notifyAlert,
		ModelOverrides: modelOverrides,
	})

	errCh := make(chan error, 1)
	// GoSafe, not a bare "go": net/http recovers panics inside its own handler
	// goroutines, but ListenAndServe itself is called from here, so a panic in
	// the accept loop would otherwise kill the process without reaching errCh.
	core.GoSafe("http server", func(msg string) { logger.Printf("%s", msg) }, func() {
		ln, err := listenWithBindRetry(cfg.Listen, bindRetryWindow())
		if err != nil {
			errCh <- err
			return
		}
		logger.Printf("client2api %s listening on http://%s  (panel: http://%s/panel/)", version, cfg.Listen, cfg.Listen)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The notification-area icon is the handle a shortcut-launched gateway
	// otherwise lacks: the console is hidden, so without it a running process
	// has no visible sign and no way back to the panel.  Quit from its menu
	// has to mean exactly what Ctrl+C means, so it cancels the context every
	// other component already watches instead of growing a second shutdown
	// path.  Headless runs (the container, CI, a service host) turn it off
	// with -tray=false.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *trayFlag {
		dataDir := cfg.DataDir
		if abs, absErr := filepath.Abs(dataDir); absErr == nil {
			dataDir = abs
		}
		link := panelURL(cfg.Listen)
		// The settings dialog edits the three connection-level keys.  What it
		// loads is the running process's own view, moved forward by every save
		// made in this process, so reopening the dialog before a restart shows
		// the pending values rather than the ones the process still uses.
		var settingsMu sync.Mutex
		pending := tray.Settings{Listen: cfg.Listen, DataDir: cfg.DataDir, Proxy: cfg.Proxy}
		loadSettings := func() (tray.Settings, error) {
			settingsMu.Lock()
			defer settingsMu.Unlock()
			return pending, nil
		}
		saveSettings := func(next tray.Settings) error {
			settingsMu.Lock()
			defer settingsMu.Unlock()
			changed, err := panel.WriteConfigKeys(absConfigPath, map[string]any{
				"listen":   next.Listen,
				"data_dir": next.DataDir,
				"proxy":    next.Proxy,
			})
			if err != nil {
				return err
			}
			pending = next
			if len(changed) > 0 {
				logger.Printf("tray settings: saved %s (restart to apply)", strings.Join(changed, ", "))
			}
			return nil
		}
		icon := tray.Start(tray.Options{
			Title:           fmt.Sprintf("client2api %s %s", version, link),
			Version:         version,
			PanelURL:        link,
			DataDir:         dataDir,
			OnRestart:       restart,
			LoadSettings:    loadSettings,
			SaveSettings:    saveSettings,
			FullSettingsURL: link + "#config",
			OnExit:          cancel,
			Logf:            logger.Printf,
		})
		trayIconMu.Lock()
		trayIcon = icon
		trayIconMu.Unlock()
		defer func() {
			trayIconMu.Lock()
			trayIcon = nil
			trayIconMu.Unlock()
			icon.Stop()
		}()
	}

	// The timetable runs for the life of the process.  An unconfigured
	// scheduler blocks here without touching a vendor, which is the behaviour a
	// default install should have.
	//
	// GoSafe keeps a panic in a batch from killing the gateway, and schedDone
	// lets the shutdown below join the goroutine: without that, a process
	// exiting while a batch was mid-flight simply abandoned it.
	schedDone := make(chan struct{})
	core.GoSafe("scheduler", func(msg string) { logger.Printf("%s", msg) }, func() {
		defer close(schedDone)
		sched.Run(ctx)
	})

	select {
	case err := <-errCh:
		usage.Stop()
		return err
	case <-ctx.Done():
		logger.Printf("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	// Stop after the server so the last requests are accounted for, and before
	// returning so the final flush is not racing the process exit.
	usage.Stop()
	// ctx is already cancelled by the time we reach here, so Run returns
	// promptly; the wait is bounded so a wedged vendor call inside a batch
	// cannot keep the process alive.
	select {
	case <-schedDone:
	case <-time.After(5 * time.Second):
		logger.Printf("scheduler did not stop within 5s; exiting anyway")
	}
	return err
}

// panelURL turns the listen address into something a browser can open.  A
// wildcard bind is not a browsable host, so it is reported as loopback --
// which is where the operator running this icon is sitting anyway.
func panelURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/panel/"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/panel/"
}

// loadConfig reads the config file and overlays the environment, creating the
// file when it is missing.
//
// A missing file is not an error for the boot path: a fresh install is meant
// to start with module defaults.  In that one case the generated api_key is
// persisted, so the operator is never left with an unauthenticated gateway
// that silently becomes authenticated (or vice versa) on the next start.  An
// existing file with an empty key is left alone and warned about instead,
// because rewriting a live deployment's credentials under it is worse than a
// loud log line.
//
// created reports whether this call wrote the file.
func loadConfig(path, ver string) (*fileConfig, bool, error) {
	return loadConfigAt(path, ver, true)
}

// loadConfigAt is loadConfig with the fresh-install behaviour made explicit.
// With create=false a missing file is an error, not a licence to generate a
// new api_key: that is what reload needs (the old code minted a fresh key and
// wrote it to disk when the file vanished under a running process, so a
// "reload" quietly changed the credential the gateway accepts) and what an
// explicitly named -config needs (a typo must not create a second
// installation in the working directory).
func loadConfigAt(path, ver string, create bool) (*fileConfig, bool, error) {
	cfg := &fileConfig{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			cfg.APIKey = newAPIKey()
			// The file is written before the environment is applied, so a
			// deployment that injects its key as a container secret does not
			// have that secret copied onto disk: the file keeps a generated key
			// and the environment decides what this process actually accepts.
			if werr := writeConfig(path, cfg, ver); werr != nil {
				// Losing the file is not fatal: the key is still in memory and
				// printed below, so the operator can act on it.
				fmt.Fprintf(os.Stderr, "client2api: could not write %s: %v\n", path, werr)
			}
			generated := cfg.APIKey
			cfg.applyEnv()
			if cfg.APIKey != generated {
				// Printing the generated key would be actively misleading: it
				// is on disk but this process does not accept it.
				fmt.Printf("client2api: api_key taken from the environment; %s holds a generated key that is not in use\n", path)
			} else {
				fmt.Printf("client2api: generated api_key: %s\n", cfg.APIKey)
			}
			return cfg, true, nil
		}
		return nil, false, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	var rawSections map[string]json.RawMessage
	if err := json.Unmarshal(b, &rawSections); err != nil {
		return nil, false, err
	}
	if raw, ok := rawSections["sources"]; ok {
		sources, err := core.ParseSources(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		cfg.Sources = sources
	}
	cfg.applyEnv()
	return cfg, false, nil
}

// writeConfig persists the minimal starting config.  It is only ever called
// when the file did not exist.
func writeConfig(path string, cfg *fileConfig, ver string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := core.EnsureDir(dir); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

// newAPIKey returns a fresh 32-hex-character bearer, drawn from crypto/rand.
// 16 random bytes is the reference's choice and is far above any brute-force
// budget for an online endpoint.
func newAPIKey() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A failure here would be a broken OS entropy source; falling back to
		// the timestamp is still better than an empty key.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// newHTTPClient builds the shared client.  No total timeout is set, because
// streaming responses legitimately outlive any fixed budget; modules apply
// their own per-request contexts.
func newHTTPClient(proxy string) (*http.Client, error) {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy %q: %w", proxy, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

// bindRetryWindow is how long a starting process keeps trying to take the
// listen address.  Only the replacement half of a panel restart waits: a
// hand-off starts while the process it replaces is still holding the port, so
// failing fast there would turn "restart" into "stop".  A normal start keeps
// the old behaviour and reports a real port conflict immediately.
func bindRetryWindow() time.Duration {
	if os.Getenv(restartHandoffEnv) != "" {
		return 30 * time.Second
	}
	return 0
}

// listenWithBindRetry opens the listen socket, retrying while the address is
// held by somebody else.  A zero window tries exactly once.
func listenWithBindRetry(addr string, window time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(window)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("listening on %s: %w", addr, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// priorityWindowConfig is one local-time override for a platform priority.
// Times are HH:MM in Beijing time; a start later than the end crosses
// midnight (for example 23:00 -> 06:00).
type priorityWindowConfig struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Priority int    `json:"priority"`
}

// parseClockMinute parses HH:MM into minutes from midnight.  The accepted
// range is 00:00 through 23:59.
func parseClockMinute(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	hour, err := strconv.Atoi(s[:2])
	if err != nil {
		return 0, false
	}
	minute, err := strconv.Atoi(s[3:])
	if err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func projectPrioritySchedule(raw []priorityWindowConfig) []core.PriorityWindow {
	out := make([]core.PriorityWindow, 0, len(raw))
	for _, w := range raw {
		start, ok := parseClockMinute(w.Start)
		if !ok {
			continue
		}
		end, ok := parseClockMinute(w.End)
		if !ok || start == end {
			continue
		}
		out = append(out, core.PriorityWindow{StartMinute: start, EndMinute: end, Priority: w.Priority})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
