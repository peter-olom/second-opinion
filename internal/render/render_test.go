package render

import (
	"strings"
	"testing"
	"time"

	"github.com/peter-olom/second-opinion/internal/config"
	"github.com/peter-olom/second-opinion/internal/panel"
)

func anonymousResult() panel.Result {
	return panel.Result{
		Mode:      "review",
		Anonymous: true,
		Opinions: []panel.Opinion{
			{Label: "Panelist A", Panelist: &config.Panelist{Backend: "codex", Model: "gpt"}, Output: "looks fine"},
			{Label: "Panelist B", Panelist: &config.Panelist{Backend: "pi", Model: "ollama/glm"}, Output: "found a bug"},
		},
		Synthesis: &panel.Opinion{Label: "Chair", Panelist: &config.Panelist{Backend: "claude", Model: "opus"}, Output: "merged"},
	}
}

func TestMarkdownHidesIdentitiesUnlessRevealed(t *testing.T) {
	res := anonymousResult()
	res.Redact()
	var b strings.Builder
	if err := Markdown(&b, res); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, leak := range []string{"codex", "ollama", "opus", "Panel key"} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted output contains %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "### Panelist B") {
		t.Errorf("output should use anonymous labels:\n%s", out)
	}
}

func TestMarkdownRevealAppendsKey(t *testing.T) {
	var b strings.Builder
	if err := Markdown(&b, anonymousResult()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	key := out[strings.Index(out, "## Panel key"):]
	for _, want := range []string{"- Panelist A: `codex:gpt`", "- Panelist B: `pi:ollama/glm`", "- Chair: `claude:opus`"} {
		if !strings.Contains(key, want) {
			t.Errorf("key missing %q:\n%s", want, key)
		}
	}
}

func TestJSONRedacted(t *testing.T) {
	res := anonymousResult()
	res.Redact()
	var b strings.Builder
	if err := JSON(&b, res); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"panelist"`) || strings.Contains(b.String(), "codex") {
		t.Errorf("redacted JSON leaks identities:\n%s", b.String())
	}
}

func TestStatusLines(t *testing.T) {
	ok := panel.Opinion{Duration: 12 * time.Second}
	fellBack := panel.Opinion{Duration: 20 * time.Second, Attempts: []panel.Attempt{{Kind: panel.KindQuota}}}
	failed := panel.Opinion{Duration: 2 * time.Second, Error: "401", ErrorKind: panel.KindAuth}
	for o, want := range map[*panel.Opinion]string{
		&ok:       "12s",
		&fellBack: "20s, answered by fallback after 1 usage limit",
		&failed:   "failed: auth, after 2s",
	} {
		if got := status(*o); got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
	}
}

func TestFailureShowsHint(t *testing.T) {
	res := panel.Result{Mode: "general", Opinions: []panel.Opinion{
		{Label: "Panelist A", Error: "429 Too Many Requests", ErrorKind: panel.KindQuota},
	}}
	var b strings.Builder
	Markdown(&b, res)
	if !strings.Contains(b.String(), "fallback model") {
		t.Errorf("quota failure should suggest a fallback:\n%s", b.String())
	}
}

func TestNamedPanelShowsWhichFallbackAnswered(t *testing.T) {
	primary := &config.Panelist{Backend: "codex", Model: "gpt"}
	res := panel.Result{Opinions: []panel.Opinion{
		{Label: "codex:gpt", Panelist: &config.Panelist{Backend: "pi", Model: "glm"}, Output: "ok",
			Attempts: []panel.Attempt{{Panelist: primary, Kind: panel.KindQuota}}},
		{Label: "claude:opus", Panelist: &config.Panelist{Backend: "claude", Model: "opus"}, Output: "ok"},
	}}
	var b strings.Builder
	if err := Markdown(&b, res); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "- codex:gpt: `pi:glm` (after `codex:gpt` hit a quota limit)") {
		t.Errorf("a named panel should say which fallback answered:\n%s", out)
	}
	res.Opinions[0].Attempts = nil
	b.Reset()
	Markdown(&b, res)
	if strings.Contains(b.String(), "Panel key") {
		t.Errorf("a named panel without fallbacks needs no key:\n%s", b.String())
	}
}
