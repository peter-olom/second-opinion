package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/config"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		msg  string
		want ErrorKind
	}{
		{"claude: error_during_execution: Claude AI usage limit reached|1760540400", KindQuota},
		{"codex: exit status 1\nERROR: exceeded retry limit, last status: 429 Too Many Requests", KindQuota},
		{"pi: exit status 1\n529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}", KindQuota},
		{"claude: exit status 1\nInvalid API key · Please run /login", KindAuth},
		{"codex: exit status 1\nNot logged in. Run `codex login`.", KindAuth},
		{"pi: exit status 1\n401 Unauthorized", KindAuth},
		{`pi: exit status 1` + "\n" + `410: {"message":"deepseek-v4-flash:0731 was retired on September 25, 2026."}`, KindModel},
		{"claude: exit status 1\nThere's an issue with the selected model (opus-9). It may not exist or you may not have access to it.", KindModel},
		{"codex: exit status 1\nunknown model gpt-9", KindModel},
		// Real messages captured from each CLI.
		{"claude: exit status 1\n[claude-code:unrecognized_model] {\"model\":\"opus-9-nonexistent\",\"query_source\":\"sdk\"}", KindModel},
		{`codex: exit status 1` + "\n" + `ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-nonexistent' model is not supported when using Codex with a ChatGPT account."}}`, KindModel},
		{"pi: exit status 1\nsegmentation fault", KindOther},
		{"used 4290 tokens then exited", KindOther}, // numbers inside other numbers are not status codes
	}
	for _, tt := range tests {
		if got := classify(errors.New(tt.msg)); got != tt.want {
			t.Errorf("classify(%q) = %s, want %s", tt.msg, got, tt.want)
		}
	}
	if got := classify(fmt.Errorf("wrapped: %w", backend.ErrTimeout)); got != KindTimeout {
		t.Errorf("timeout classified as %s", got)
	}
	if got := classify(context.Canceled); got != KindCancelled {
		t.Errorf("cancellation classified as %s", got)
	}
}

// limitedCLI fails with a usage limit for model "limited", an auth error for
// "signedout", and answers for anything else.
func limitedCLI(t *testing.T) string {
	return fakeCLI(t, `
while [ $# -gt 0 ]; do [ "$1" = --model ] && model=$2; shift; done
cat >/dev/null
case "$model" in
  limited) echo "429 rate limit exceeded" >&2; exit 1;;
  signedout) echo "401 Unauthorized" >&2; exit 1;;
  *) echo "answer from $model";;
esac`)
}

func TestFallbackOnQuota(t *testing.T) {
	runner := &Runner{Backends: map[string]config.Backend{"pi": {Command: limitedCLI(t)}}}
	pl := config.Panelist{Backend: "pi", Model: "limited", Fallback: []config.Panelist{
		{Backend: "pi", Model: "limited"},
		{Backend: "pi", Model: "spare"},
	}}

	op := runner.Ask(context.Background(), pl, "Panelist A", "sys", "hi", "")

	if !op.OK() || op.Output != "answer from spare" {
		t.Fatalf("op = %+v, want the second fallback's answer", op)
	}
	if op.Panelist.Model != "spare" || len(op.Panelist.Fallback) != 0 {
		t.Errorf("Panelist = %+v, want the model that answered", op.Panelist)
	}
	if len(op.Attempts) != 2 || op.Attempts[0].Kind != KindQuota || op.Attempts[0].Panelist.Model != "limited" {
		t.Errorf("attempts = %+v, want two quota failures", op.Attempts)
	}

	res := Result{Anonymous: true, Opinions: []Opinion{op}}
	res.Redact()
	if a := res.Opinions[0].Attempts[0]; a.Panelist != nil || a.Error != "" || a.Kind != KindQuota {
		t.Errorf("redacted attempt = %+v, want only its kind", a)
	}
}

func TestNoFallbackForOtherErrors(t *testing.T) {
	runner := &Runner{Backends: map[string]config.Backend{"pi": {Command: limitedCLI(t)}}}
	pl := config.Panelist{Backend: "pi", Model: "signedout", Fallback: []config.Panelist{{Backend: "pi", Model: "spare"}}}

	op := runner.Ask(context.Background(), pl, "Panelist A", "sys", "hi", "")

	if op.OK() || op.ErrorKind != KindAuth || len(op.Attempts) != 0 {
		t.Errorf("op = %+v, want an auth failure without trying the fallback", op)
	}
	if !strings.Contains(op.ErrorKind.Hint(), "sign in") {
		t.Errorf("auth hint = %q", op.ErrorKind.Hint())
	}
}

func TestQuotaWithoutFallbackFails(t *testing.T) {
	runner := &Runner{Backends: map[string]config.Backend{"pi": {Command: limitedCLI(t)}}}
	op := runner.Ask(context.Background(), config.Panelist{Backend: "pi", Model: "limited"}, "Panelist A", "sys", "hi", "")
	if op.OK() || op.ErrorKind != KindQuota || len(op.Attempts) != 0 {
		t.Errorf("op = %+v, want a quota failure", op)
	}
}

func TestClassifyContextBeforeQuota(t *testing.T) {
	for _, msg := range []string{
		"claude: error: prompt is too long: 250000 tokens > 200000 maximum",
		"codex: ERROR: Your input exceeds the context window of this model",
		"pi: 400 token limit exceeded",
	} {
		if got := classify(errors.New(msg)); got != KindContext {
			t.Errorf("classify(%q) = %s, want context", msg, got)
		}
	}
}

func TestQuotaAtTheEndBeatsTranscriptMentions(t *testing.T) {
	msg := "codex: exit status 1\n" +
		"exec rg 'context window' internal/panel/errors.go\n" +
		"KindContext ... context window|prompt is too long ...\n" +
		"ERROR: exceeded retry limit, last status: 429 Too Many Requests"
	if got := classify(errors.New(msg)); got != KindQuota {
		t.Errorf("classify = %s, want quota so a fallback runs", got)
	}
}
