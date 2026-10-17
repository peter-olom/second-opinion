package panel

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/peter-olom/second-opinion/internal/backend"
)

// ErrorKind says why a model call failed, so the caller knows what to do about it.
type ErrorKind string

const (
	KindContext   ErrorKind = "context"   // the prompt is too long for the model
	KindQuota     ErrorKind = "quota"     // usage or rate limit, or the provider is overloaded
	KindAuth      ErrorKind = "auth"      // not signed in, or credentials rejected
	KindModel     ErrorKind = "model"     // unknown, retired, or unsupported model
	KindTimeout   ErrorKind = "timeout"   // exceeded the per-model timeout
	KindCancelled ErrorKind = "cancelled" // the run was interrupted
	KindOther     ErrorKind = "other"
)

// Kinds lists every kind; keep it next to the constants above.
var Kinds = []ErrorKind{KindContext, KindQuota, KindAuth, KindModel, KindTimeout, KindCancelled, KindOther}

// The CLIs report failures as free text, so classification matches common
// phrasings and HTTP status codes. Order matters:
//   - unambiguous rate-limit signals come first, so a transcript that happens
//     to mention "context window" cannot hide a 429 (and block a fallback);
//   - "token limit exceeded" is a context error, so context comes before the
//     broader quota phrasings;
//   - a quota message can mention the user's plan or login, so quota comes
//     before auth.
var kindPatterns = []struct {
	kind ErrorKind
	re   *regexp.Regexp
}{
	{KindQuota, regexp.MustCompile(`(?i)\b429\b|rate.?limit|too many requests|usage limit`)},
	{KindContext, regexp.MustCompile(`(?i)context (length|window)|prompt is too long|input is too long|maximum context|too many (input )?tokens|token limit`)},
	{KindQuota, regexp.MustCompile(`(?i)quota|limit (reached|exceeded)|insufficient (credit|balance|funds)|credit balance|out of credits|overloaded|\b529\b|\b503\b|capacity`)},
	{KindAuth, regexp.MustCompile(`(?i)not logged in|please (log|sign) ?in|/login|unauthori[sz]ed|\b401\b|\b403\b|authenticat|invalid (api )?key|api key|credential|(token|session) (has )?expired|forbidden`)},
	{KindModel, regexp.MustCompile(`(?i)retired|deprecated|model.{0,40}(not found|does not exist|not supported|not available|unavailable|unknown|invalid)|(unknown|invalid|unsupported|no such) model|issue with the selected model|unrecogni[sz]ed.?model|\b404\b|\b410\b`)},
}

// classify sorts a failure. The last line of the message is tried first,
// since that is where a CLI prints its actual error, after any transcript.
func classify(err error) ErrorKind {
	switch {
	case errors.Is(err, backend.ErrTimeout):
		return KindTimeout
	case errors.Is(err, context.Canceled):
		return KindCancelled
	}
	// The first line is so's own "<cli>: exit status N"; the last is usually
	// the CLI's real error. Lines in between are often transcript (code,
	// prompts, tool output), which can mention "429" or "api key" without
	// being the error, so only lines that look like errors are considered.
	msg := strings.TrimSpace(err.Error())
	lines := strings.Split(msg, "\n")
	if k := match(lines[len(lines)-1]); k != KindOther {
		return k
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if errorLine.MatchString(lines[i]) {
			if k := match(lines[i]); k != KindOther {
				return k
			}
		}
	}
	return KindOther
}

// errorLine matches lines that state an error rather than quote one.
var errorLine = regexp.MustCompile(`(?i)^\s*(\S+:\s+)?(error|fatal|failed)\b|^\s*\{"type":\s*"error"|^[45]\d\d(?-i: [A-Z][A-Za-z]|: ?\{| \{)|^\s*\S+: exit status`)

func match(s string) ErrorKind {
	for _, p := range kindPatterns {
		if p.re.MatchString(s) {
			return p.kind
		}
	}
	return KindOther
}

// Hint is what the caller should do about a failure of this kind.
func (k ErrorKind) Hint() string {
	switch k {
	case KindQuota:
		return "The provider's usage or rate limit was hit. Wait and retry, or give this panelist a fallback model in the profile."
	case KindAuth:
		return "The backend CLI is not signed in or its credentials were rejected. Ask the user to sign in (claude auth login, codex login, or the pi provider's login)."
	case KindModel:
		return "The model is unknown, retired, or unsupported. Update the profile; `so doctor --with backend:model` tests a replacement."
	case KindContext:
		return "The prompt is too long for this model. Attach less, for example a narrower --base or fewer -f files."
	case KindTimeout:
		return "The model did not finish in time. Retry with a longer --timeout or a lower effort."
	case KindCancelled:
		return "The run was interrupted before this model answered. Run it again if you still need its answer."
	}
	return ""
}
