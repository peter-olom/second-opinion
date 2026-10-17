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
	for _, mode := range []string{"general", "review", "plan", "debug", "audit"} {
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
	no := false
	modes := map[string]Mode{"tricky": {
		Description:       `quotes " and \ backslash`,
		Instructions:      "has ''' triple quotes\nand a trailing newline\n",
		Format:            "## Answer\n\ttabbed",
		ChairInstructions: "chair",
		ChairFormat:       "chair format",
		Repo:              &no,
	}}
	for _, name := range cfg.ModeNames() {
		modes[name] = cfg.Modes[name]
	}
	for name, want := range modes {
		// A new name, so no built-in fills in a field TOML() left out.
		rt := "rt-" + name
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(want.TOML(rt)), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, want.TOML(rt))
		}
		if g := got.Modes[rt]; !reflect.DeepEqual(g, want) {
			t.Errorf("mode %s did not round-trip through TOML:\n got %+v\nwant %+v", name, g, want)
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

func TestDefaultChairNestedFallbackRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`default_chair = { backend = "pi", fallback = [{ backend = "pi", fallback = [{ backend = "pi" }] }] }`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("nested fallback on default_chair should be rejected")
	}
}

func TestParseSpecRejectsEffortTypos(t *testing.T) {
	for _, spec := range []string{"codex:gpt@High", "codex:gpt@ultra", "pi:ollama/x:cloud@hgih"} {
		if _, err := ParseSpec(spec); err == nil || !strings.Contains(err.Error(), "invalid effort") {
			t.Errorf("ParseSpec(%q) err = %v, want an invalid-effort error", spec, err)
		}
	}
}

func TestParseSpecTrailingAt(t *testing.T) {
	got, err := ParseSpec("codex:gpt@")
	if err != nil || got.Model != "gpt" || got.Effort != "" {
		t.Errorf("ParseSpec(codex:gpt@) = %+v, %v; want model gpt, no effort", got, err)
	}
}

func TestSeverityModesDefineTheirScale(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// Panelists get only their own mode's text, so a mode whose format asks
	// for severities has to define them.
	checked := 0
	defer func() {
		if checked < 2 {
			t.Errorf("checked %d modes; review and audit should both ask for severities", checked)
		}
	}()
	for _, name := range cfg.ModeNames() {
		m := cfg.Modes[name]
		if !strings.Contains(m.Format, "severity") {
			continue
		}
		checked++
		for _, want := range []string{"blocker:", "major:", "minor:", "nit:", "Verdict: approve if"} {
			if !strings.Contains(m.Instructions, want) {
				t.Errorf("mode %s asks for severities but its instructions lack %q", name, want)
			}
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, tc := range map[string]struct{ toml, want string }{
		"profile effort":     {"[profiles.p]\npanelists = [{ backend = \"pi\", effort = \"hgih\" }]\n", `unknown effort "hgih"`},
		"chair effort":       {"[profiles.p]\npanelists = [{ backend = \"pi\" }]\nchair = { backend = \"pi\", effort = \"hgih\" }\n", `profile "p" chair: unknown effort`},
		"fallback effort":    {"[profiles.p]\npanelists = [{ backend = \"pi\", fallback = [{ backend = \"pi\", effort = \"hgih\" }] }]\n", `unknown effort "hgih"`},
		"default chair":      {"default_chair = { backend = \"pi\", effort = \"hgih\" }\n", `default_chair: unknown effort`},
		"history_days 0":     {"history_days = 0\n", "history_days = 0 is not allowed"},
		"dash model":         {"[profiles.p]\npanelists = [{ backend = \"pi\", model = \"--tools\" }]\n", "can't start with -"},
		"dash provider":      {"[profiles.p]\npanelists = [{ backend = \"pi\", provider = \"-x\" }]\n", "can't start with -"},
		"misspelled backend": {"[backends.cluade]\nenv = { ANTHROPIC_API_KEY = \"\" }\n", "unknown backend [backends.cluade]"},
		"control name":       {"[profiles.p]\npanelists = [{ backend = \"pi\", name = \"a\\u001b[2Jb\" }]\n", "control characters"},
		"relative command":   {"[backends.pi]\ncommand = \"node_modules/.bin/pi\"\n", "use a command name on PATH or an absolute path"},
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(tc.toml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[profiles.p]\npanelists = [{ backend = \"pi\", effort = \"high\" }]\n"), 0o644)
	if _, err := Load(path); err != nil {
		t.Errorf("a valid effort was rejected: %v", err)
	}
}

func TestParseSpecRejectsFlagLikeModels(t *testing.T) {
	if _, err := ParseSpec("pi:--tools"); err == nil {
		t.Error("a model that starts with - should be rejected")
	}
}

func TestNoConfigWithoutAbsoluteHome(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.MkdirAll(filepath.Join(dir, ".config", "second-opinion"), 0o755)
	os.WriteFile(filepath.Join(dir, ".config", "second-opinion", "config.toml"), []byte("[backends.pi]\ncommand = \"/tmp/evil\"\n"), 0o644)
	t.Setenv("SECOND_OPINION_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, home := range []string{".", ""} {
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if p := DefaultPath(); p != "" {
			t.Errorf("HOME=%q: DefaultPath = %q, want none", home, p)
		}
		cfg, err := Load(DefaultPath())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backends["pi"].Command != "" {
			t.Errorf("HOME=%q: a config in the current directory was loaded", home)
		}
	}
}

func TestParseSpecRejectsControlCharacters(t *testing.T) {
	if _, err := ParseSpec("pi:x\n## Panel key"); err == nil {
		t.Error("a spec with a newline should be rejected")
	}
}
