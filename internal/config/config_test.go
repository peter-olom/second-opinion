package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peter-olom/second-opinion/internal/assets"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		spec string
		want Panelist
	}{
		{"claude", Panelist{Backend: "claude"}},
		{"claude:opus", Panelist{Backend: "claude", Model: "opus"}},
		{"codex:gpt-6.1-sol@high", Panelist{Backend: "codex", Model: "gpt-6.1-sol", Effort: "high"}},
		{"pi:ollama/glm-5.3:cloud@max", Panelist{Backend: "pi", Model: "ollama/glm-5.3:cloud", Effort: "max"}},
	}
	for _, tt := range tests {
		got, err := ParseSpec(tt.spec)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", tt.spec, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
		}
		if got.Spec() != tt.spec {
			t.Errorf("Spec() round trip = %q, want %q", got.Spec(), tt.spec)
		}
	}
	if _, err := ParseSpec(":opus"); err == nil {
		t.Error("ParseSpec(\":opus\") should fail without a backend")
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exists {
		t.Error("Exists = true, want false when no user config exists")
	}
	if len(cfg.Profiles) != 0 {
		t.Errorf("profiles = %v, want none shipped", cfg.ProfileNames())
	}
	if _, _, err := cfg.Profile(""); !errors.Is(err, ErrNoProfiles) {
		t.Errorf("Profile(\"\") err = %v, want ErrNoProfiles", err)
	}
	for _, mode := range []string{"general", "review", "plan", "debug"} {
		if _, ok := cfg.Modes[mode]; !ok {
			t.Errorf("built-in mode %q missing", mode)
		}
	}
}

func TestLoadMergesUserConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	user := `
default_profile = "solo"

[profiles.solo]
panelists = [{ backend = "claude", model = "opus" }]

[modes.review]
format = "## Custom review format"

[modes.security]
description = "Security audit"
instructions = "Look for vulnerabilities."
`
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProfileNames(); len(got) != 1 || got[0] != "solo" {
		t.Errorf("profiles = %v, want user profiles to replace the built-ins", got)
	}
	review := cfg.Modes["review"]
	if review.Format != "## Custom review format" {
		t.Errorf("review format not overridden: %q", review.Format)
	}
	if !strings.Contains(review.Instructions, "failure scenario") {
		t.Error("review instructions should be inherited from the built-in mode")
	}
	if cfg.Modes["security"].Instructions == "" {
		t.Error("user-defined mode missing")
	}
	if review.Repo == nil || !*review.Repo {
		t.Error("review should keep its built-in repo setting")
	}
	if _, ok := cfg.Modes["plan"]; !ok {
		t.Error("built-in modes should remain available")
	}
	if !cfg.Customized["review"] || !cfg.Customized["security"] || cfg.Customized["plan"] {
		t.Errorf("customized = %v, want review and security only", cfg.Customized)
	}
	if !cfg.Builtin["review"] || cfg.Builtin["security"] {
		t.Errorf("builtin = %v", cfg.Builtin)
	}
}

func TestStarterConfigDoesNotFreezeModes(t *testing.T) {
	// `so init` writes assets.Defaults. If it contained modes, a user's config
	// would shadow every built-in mode and never receive updates.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(assets.Defaults), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Customized) != 0 {
		t.Errorf("starter config customizes modes %v; it should leave them built-in", cfg.Customized)
	}
}

func TestModeTOMLRoundTrip(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cfg.ModeNames() {
		want := cfg.Modes[name]
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(want.TOML(name)), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, want.TOML(name))
		}
		if g := got.Modes[name]; g.Format != want.Format || g.Instructions != want.Instructions ||
			g.ChairFormat != want.ChairFormat || g.ChairInstructions != want.ChairInstructions {
			t.Errorf("mode %s did not round-trip through TOML", name)
		}
	}
}

func TestProfileResolution(t *testing.T) {
	one := Profile{Panelists: []Panelist{{Backend: "claude"}}}
	tests := []struct {
		name     string
		cfg      Config
		ask      string
		want     string
		errMatch string
	}{
		{"only profile is the default", Config{Profiles: map[string]Profile{"solo": one}}, "", "solo", ""},
		{"default_profile wins", Config{DefaultProfile: "b", Profiles: map[string]Profile{"a": one, "b": one}}, "", "b", ""},
		{"explicit name", Config{DefaultProfile: "b", Profiles: map[string]Profile{"a": one, "b": one}}, "a", "a", ""},
		{"ambiguous default", Config{Profiles: map[string]Profile{"a": one, "b": one}}, "", "", "no default_profile"},
		{"unknown", Config{Profiles: map[string]Profile{"a": one}}, "zzz", "", "unknown profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := tt.cfg.Profile(tt.ask)
			if tt.errMatch != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errMatch) {
					t.Errorf("err = %v, want %q", err, tt.errMatch)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Profile(%q) = %q, %v; want %q", tt.ask, got, err, tt.want)
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`defualt_profile = "quick"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "defualt_profile") {
		t.Errorf("err = %v, want unknown key error", err)
	}
}

func TestDefaultPathIgnoresRelativeXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SECOND_OPINION_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", ".")
	if got, want := DefaultPath(), filepath.Join(home, ".config/second-opinion/config.toml"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME: DefaultPath() = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "/etc/xdg")
	if got := DefaultPath(); got != "/etc/xdg/second-opinion/config.toml" {
		t.Errorf("absolute XDG_CONFIG_HOME: DefaultPath() = %q", got)
	}
}

func TestValidationRejectsDangerousValues(t *testing.T) {
	for name, body := range map[string]string{
		"huge history_days": "history_days = 999999\n",
		"zero timeout":      "timeout = \"0s\"\n",
		"negative timeout":  "timeout = \"-5m\"\n",
		"nested fallback": `[profiles.p]
panelists = [{ backend = "pi", model = "a", fallback = [{ backend = "pi", model = "b", fallback = [{ backend = "pi", model = "c" }] }] }]
`,
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: config should be rejected", name)
		}
	}
}

func TestParseSpecKeepsAtSignsInModelNames(t *testing.T) {
	got, err := ParseSpec("claude:claude-opus@20250514")
	if err != nil || got.Model != "claude-opus@20250514" || got.Effort != "" {
		t.Errorf("ParseSpec = %+v, %v; the date is part of the model", got, err)
	}
	got, _ = ParseSpec("claude:claude-opus@20250514@high")
	if got.Model != "claude-opus@20250514" || got.Effort != "high" {
		t.Errorf("ParseSpec = %+v; want model with date and effort high", got)
	}
}
