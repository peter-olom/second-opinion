// Package config loads profiles, modes, and backend settings.
//
// The built-in settings and modes are always loaded first. No profiles ship
// with so, since they depend on what the user has installed; they come only
// from the user config. The user config also overrides settings and backends,
// and merges into modes field by field, so a user can replace just a mode's
// format.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/peter-olom/second-opinion/internal/assets"
)

// Panelist is one model taking part in a panel, or a panel's chair.
type Panelist struct {
	Name     string `toml:"name" json:"name,omitempty"`
	Backend  string `toml:"backend" json:"backend"`
	Model    string `toml:"model" json:"model,omitempty"`
	Effort   string `toml:"effort" json:"effort,omitempty"`
	Provider string `toml:"provider" json:"provider,omitempty"`
	// Fallback lists models to try, in order, when this one hits a usage or rate limit.
	Fallback []Panelist `toml:"fallback" json:"fallback,omitempty"`
}

// Label is how the panelist is identified in output.
func (p Panelist) Label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Display()
}

// Display is Spec plus the provider, which Spec can't carry, so two pi
// panelists on different providers stay distinguishable.
func (p Panelist) Display() string {
	if p.Provider != "" {
		return p.Spec() + " (provider " + p.Provider + ")"
	}
	return p.Spec()
}

// Spec renders the panelist in the backend:model@effort form accepted by ParseSpec.
func (p Panelist) Spec() string {
	s := p.Backend
	if p.Model != "" {
		s += ":" + p.Model
	}
	if p.Effort != "" {
		s += "@" + p.Effort
	}
	return s
}

func isWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

var effortLevels = map[string]bool{
	"off": true, "none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// ParseSpec parses "backend[:model][@effort]", e.g. "pi:ollama/glm-5.3:cloud@high".
func ParseSpec(spec string) (Panelist, error) {
	var p Panelist
	s := spec
	// Only a known level after the last "@" is an effort; model IDs can
	// contain "@" themselves (for example claude-opus@20250514). A word that
	// is not a level is most likely a typo, so it is an error rather than
	// part of the model name.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		switch suffix := s[i+1:]; {
		case suffix == "":
			s = s[:i] // a trailing "@" means no effort
		case effortLevels[suffix]:
			p.Effort, s = suffix, s[:i]
		case isWord(suffix):
			return p, fmt.Errorf("invalid effort %q in %q: want one of off, none, minimal, low, medium, high, xhigh, max", suffix, spec)
		}
	}
	p.Backend, p.Model, _ = strings.Cut(s, ":")
	if p.Backend == "" {
		return p, fmt.Errorf("invalid panelist %q: want backend[:model][@effort]", spec)
	}
	if strings.HasPrefix(p.Model, "-") {
		return p, fmt.Errorf("invalid model %q in %q: a model can't start with -", p.Model, spec)
	}
	if hasControl(spec) {
		return p, fmt.Errorf("invalid panelist %q: control characters", spec)
	}
	return p, nil
}

type Profile struct {
	Description string     `toml:"description"`
	Mode        string     `toml:"mode"`
	Panelists   []Panelist `toml:"panelists"`
	Chair       *Panelist  `toml:"chair"`
}

type Mode struct {
	Description       string `toml:"description"`
	Instructions      string `toml:"instructions"`
	Format            string `toml:"format"`
	ChairInstructions string `toml:"chair_instructions"`
	ChairFormat       string `toml:"chair_format"`
	// Repo gives panelists read-only access to the caller's git repository by default.
	Repo *bool `toml:"repo"`
}

type Backend struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

type Config struct {
	DefaultProfile string    `toml:"default_profile"`
	DefaultMode    string    `toml:"default_mode"`
	DefaultChair   *Panelist `toml:"default_chair"`
	Timeout        string    `toml:"timeout"`
	// Anonymize hides model names from the chair and the caller (default true).
	Anonymize *bool `toml:"anonymize"`
	// History records every run locally so it can be inspected later (default true).
	History *bool `toml:"history"`
	// HistoryDays is how many days runs are kept before being pruned (default 7).
	HistoryDays int                `toml:"history_days"`
	Backends    map[string]Backend `toml:"backends"`
	Profiles    map[string]Profile `toml:"profiles"`
	Modes       map[string]Mode    `toml:"modes"`

	// Path is the user config file, whether or not it exists yet.
	Path string `toml:"-"`
	// Exists reports whether the user config file was found and loaded.
	Exists bool `toml:"-"`
	// Builtin records the names of the modes that ship with so.
	Builtin map[string]bool `toml:"-"`
	// Customized records the modes the user config defines or overrides.
	Customized map[string]bool `toml:"-"`
}

// DefaultPath returns $SECOND_OPINION_CONFIG, or config.toml under the XDG
// config directory. A relative XDG_CONFIG_HOME is ignored, as the XDG spec requires.
func DefaultPath() string {
	if p := os.Getenv("SECOND_OPINION_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "second-opinion", "config.toml")
	}
	// Without an absolute home there is no config location. A relative one
	// would be read from the current directory, which may be an untrusted
	// checkout that then picks the backend commands. "" means built-in
	// defaults only.
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".config", "second-opinion", "config.toml")
}

// Load reads the built-in defaults and overlays the user config at path, if it exists.
func Load(path string) (*Config, error) {
	cfg, err := decode(assets.Defaults, "built-in defaults")
	if err != nil {
		return nil, err
	}
	modes, err := decode(assets.Modes, "built-in modes")
	if err != nil {
		return nil, err
	}
	cfg.Modes = modes.Modes
	cfg.Builtin = map[string]bool{}
	for name := range cfg.Modes {
		cfg.Builtin[name] = true
	}
	cfg.Customized = map[string]bool{}
	cfg.Path = path
	if path == "" {
		return cfg, cfg.validate()
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return cfg, cfg.validate()
	case err != nil:
		return nil, err
	}
	user, err := decode(string(data), path)
	if err != nil {
		return nil, err
	}
	cfg.merge(user)
	cfg.Exists = true
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func decode(data, source string) (*Config, error) {
	var c Config
	md, err := toml.Decode(data, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", source, strings.Join(names, ", "))
	}
	// 0 would otherwise read as "left out" and keep runs for 7 days.
	if md.IsDefined("history_days") && c.HistoryDays == 0 {
		return nil, fmt.Errorf("%s: history_days = 0 is not allowed; set history = false to keep no runs", source)
	}
	return &c, nil
}

func (c *Config) merge(u *Config) {
	if u.DefaultProfile != "" {
		c.DefaultProfile = u.DefaultProfile
	}
	if u.DefaultMode != "" {
		c.DefaultMode = u.DefaultMode
	}
	if u.DefaultChair != nil {
		c.DefaultChair = u.DefaultChair
	}
	if u.Timeout != "" {
		c.Timeout = u.Timeout
	}
	if u.Anonymize != nil {
		c.Anonymize = u.Anonymize
	}
	if u.History != nil {
		c.History = u.History
	}
	if u.HistoryDays != 0 {
		c.HistoryDays = u.HistoryDays
	}
	c.Profiles = u.Profiles
	if c.Backends == nil {
		c.Backends = map[string]Backend{}
	}
	for name, b := range u.Backends {
		c.Backends[name] = b
	}
	if c.Modes == nil {
		c.Modes = map[string]Mode{}
	}
	for name, m := range u.Modes {
		base := c.Modes[name]
		overlay(&base.Description, m.Description)
		overlay(&base.Instructions, m.Instructions)
		overlay(&base.Format, m.Format)
		overlay(&base.ChairInstructions, m.ChairInstructions)
		overlay(&base.ChairFormat, m.ChairFormat)
		if m.Repo != nil {
			base.Repo = m.Repo
		}
		c.Modes[name] = base
		c.Customized[name] = true
	}
}

func overlay(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func (c *Config) validate() error {
	if _, err := c.TimeoutDuration(); err != nil {
		return err
	}
	if c.HistoryDays < 0 || c.HistoryDays > 36500 {
		return fmt.Errorf("history_days must be from 1 to 36500, or left out for 7 (set history = false to turn history off)")
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile %q is not a defined profile", c.DefaultProfile)
		}
	}
	if _, ok := c.Modes[c.DefaultMode]; !ok {
		return fmt.Errorf("default_mode %q is not a defined mode", c.DefaultMode)
	}
	for name, bc := range c.Backends {
		// A misspelled section would be ignored without a word, along with
		// any env it sets (such as clearing an API key).
		if name != "claude" && name != "codex" && name != "pi" {
			return fmt.Errorf("unknown backend [backends.%s] (want claude, codex, or pi)", name)
		}
		// A relative path with a separator would resolve against the
		// panelist's working directory, the reviewed repository with repo
		// access, and run a binary from it.
		if bc.Command != "" && !filepath.IsAbs(bc.Command) && strings.ContainsAny(bc.Command, `/\`) {
			return fmt.Errorf("backends.%s.command %q: use a command name on PATH or an absolute path", name, bc.Command)
		}
	}
	check := func(where string, pl Panelist) error {
		for _, x := range append([]Panelist{pl}, pl.Fallback...) {
			// Model and provider become CLI arguments of their own; one that
			// starts with - would read as a flag.
			if hasControl(x.Name + x.Backend + x.Model + x.Provider + x.Effort) {
				return fmt.Errorf("%s: control characters in a panelist's name, backend, model, provider, or effort", where)
			}
			if strings.HasPrefix(x.Model, "-") || strings.HasPrefix(x.Provider, "-") {
				return fmt.Errorf("%s: model and provider can't start with - (%q, %q)", where, x.Model, x.Provider)
			}
			if x.Effort != "" && !effortLevels[x.Effort] {
				return fmt.Errorf("%s: unknown effort %q for %s (want one of off, none, minimal, low, medium, high, xhigh, max)", where, x.Effort, x.Backend)
			}
		}
		return nil
	}
	if c.DefaultChair != nil {
		if err := check("default_chair", *c.DefaultChair); err != nil {
			return err
		}
	}
	for name, p := range c.Profiles {
		for _, pl := range p.Panelists {
			if err := check(fmt.Sprintf("profile %q", name), pl); err != nil {
				return err
			}
		}
		if p.Chair != nil {
			if err := check(fmt.Sprintf("profile %q chair", name), *p.Chair); err != nil {
				return err
			}
		}
	}
	if c.DefaultChair != nil {
		for _, f := range c.DefaultChair.Fallback {
			if len(f.Fallback) > 0 {
				return fmt.Errorf("default_chair: fallbacks cannot have fallbacks of their own")
			}
		}
	}
	for name, p := range c.Profiles {
		if len(p.Panelists) == 0 {
			return fmt.Errorf("profile %q has no panelists", name)
		}
		all := p.Panelists
		if p.Chair != nil {
			all = append(all[:len(all):len(all)], *p.Chair)
		}
		for _, pl := range all {
			for _, f := range pl.Fallback {
				if len(f.Fallback) > 0 {
					return fmt.Errorf("profile %q: fallbacks cannot have fallbacks of their own; list them all on %s", name, pl.Label())
				}
			}
		}
		if p.Mode != "" {
			if _, ok := c.Modes[p.Mode]; !ok {
				return fmt.Errorf("profile %q uses undefined mode %q", name, p.Mode)
			}
		}
	}
	return nil
}

// Anonymous reports whether panels are anonymous, which is the default.
func (c *Config) Anonymous() bool {
	return c.Anonymize == nil || *c.Anonymize
}

// Recording reports whether runs are recorded in the local history, which is the default.
func (c *Config) Recording() bool {
	return c.History == nil || *c.History
}

// Retention is how long the history keeps a run.
func (c *Config) Retention() time.Duration {
	days := c.HistoryDays
	if days == 0 {
		days = 7
	}
	return time.Duration(days) * 24 * time.Hour
}

// ErrNoProfiles is returned when a profile is needed but none are configured.
var ErrNoProfiles = errors.New("no profiles configured")

// Profile returns the named profile, or the default profile if name is empty.
// The default is default_profile, or the only profile if there is just one.
func (c *Config) Profile(name string) (string, Profile, error) {
	if len(c.Profiles) == 0 {
		return "", Profile{}, ErrNoProfiles
	}
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" {
		if len(c.Profiles) != 1 {
			return "", Profile{}, fmt.Errorf("no default_profile set; pass --profile (available: %s)",
				strings.Join(c.ProfileNames(), ", "))
		}
		name = c.ProfileNames()[0]
	}
	p, ok := c.Profiles[name]
	if !ok {
		return "", Profile{}, fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(c.ProfileNames(), ", "))
	}
	return name, p, nil
}

// TimeoutDuration is the per-call timeout applied to each panelist and the chair.
func (c *Config) TimeoutDuration() (time.Duration, error) {
	if c.Timeout == "" {
		return 15 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", c.Timeout, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout %q must be positive", c.Timeout)
	}
	return d, nil
}

func (c *Config) ProfileNames() []string { return sortedKeys(c.Profiles) }

func (c *Config) ModeNames() []string { return sortedKeys(c.Modes) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TOML renders a mode as a config section, ready to copy into a config file and edit.
func (m Mode) TOML(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[modes.%s]\n", tomlKey(name))
	field := func(key, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s = %s\n", key, tomlValue(v))
		}
	}
	field("description", m.Description)
	if m.Repo != nil {
		fmt.Fprintf(&b, "repo = %t\n", *m.Repo)
	}
	field("instructions", m.Instructions)
	field("format", m.Format)
	field("chair_instructions", m.ChairInstructions)
	field("chair_format", m.ChairFormat)
	return b.String()
}

func tomlKey(k string) string {
	for _, r := range k {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b, _ := json.Marshal(k)
			return string(b)
		}
	}
	return k
}

// tomlValue uses a multi-line literal string for text, which needs no escaping,
// and falls back to a basic string when that is not possible. JSON string
// escapes are a subset of TOML's, so a JSON-encoded string is valid TOML.
func tomlValue(v string) string {
	if strings.Contains(v, "\n") && !strings.Contains(v, "'''") && !strings.HasSuffix(v, "'") {
		return "'''\n" + v + "'''"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// hasControl reports whether s holds a control character, which would
// garble the labels and specs so prints.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) })
}
