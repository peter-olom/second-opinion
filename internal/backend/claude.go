package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// claude runs Claude Code in print mode with no MCP servers, settings, or session.
// In a workspace it gets the read-only file tools, and Claude Code denies reads
// outside the working directory.
type claude struct{ opts Options }

func (c claude) Run(ctx context.Context, req Request) (string, error) {
	tools := ""
	if req.Workspace != "" {
		tools = "Read,Grep,Glob"
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--tools", tools,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--system-prompt", req.System + mentionNote,
	}
	if req.Workspace == "" {
		// Defense in depth for the mention expansion below: with no tools,
		// nothing may be read at all.
		args = append(args, "--settings", `{"permissions":{"deny":["Read"]}}`)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if e := claudeEffort(req.Effort); e != "" {
		args = append(args, "--effort", e)
	}

	out, runErr := run(ctx, "claude", c.opts, req.cwd(), args, defuseMentions(req.Prompt))
	var res struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	parseErr := json.Unmarshal(out, &res)
	// Undo the defusing in anything claude echoes back, so a quoted snippet
	// carries no hidden character and doesn't mark the answer as claude's.
	res.Result = restoreMentions(res.Result)
	if runErr != nil {
		// Claude reports its errors (usage limits, unknown models) in the JSON
		// result. Put that last, where classification looks first, so stderr
		// noise or a numeric JSON field cannot decide the kind.
		if parseErr == nil && res.Result != "" && (res.IsError || res.Subtype != "success") {
			return "", fmt.Errorf("%w\nclaude: %s: %s", runErr, res.Subtype, truncate([]byte(oneLine(res.Result)), 500))
		}
		// Otherwise, if the only detail is claude's own JSON output, end with
		// a neutral line so its fields (a duration of 429 ms, an answer about
		// rate limits) are not mistaken for the error.
		var cliErr *CLIError
		if parseErr == nil && errors.As(runErr, &cliErr) && cliErr.FromStdout {
			subtype := res.Subtype
			if subtype == "" {
				subtype = "no result"
			}
			return "", fmt.Errorf("%w\nclaude: %s: no error message", runErr, subtype)
		}
		return "", runErr
	}
	if parseErr != nil {
		return "", fmt.Errorf("claude: unexpected output: %s", truncate(out, 500))
	}
	if res.IsError {
		return "", fmt.Errorf("claude: %s: %s", res.Subtype, res.Result)
	}
	return res.Result, nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// claudeEffort maps to Claude Code's levels: low, medium, high, xhigh, max.
func claudeEffort(e string) string {
	switch e {
	case "off", "none", "minimal":
		return "low"
	}
	return e
}

// Claude Code expands an @path mention anywhere in a print-mode prompt (in
// code fences and tags too) by reading that file into the prompt, whatever
// --tools says and, with repo access, even outside the repository. A diff,
// an attached file, or a panelist's answer to the chair could therefore make
// claude send ~/.aws/credentials or the like to the provider. A zero-width
// space before each @ stops the expansion: the @ no longer starts a word
// (verified with Claude Code 2.1). It must go before, not after: "@" then a
// zero-width space then "/c" is still a mention, of a relative path that a
// reviewed repository can supply (a folder named U+200B holding a symlink
// out of the repository was expanded in testing). To fail closed on
// whatever counts as a word start (Unicode spaces, punctuation, a diff's
// "+"), every @ is marked unless the character before it in the original
// text is an ASCII letter or digit, as in an email address.
func defuseMentions(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prev := rune(-1)
	for _, r := range s {
		if r == '@' && !asciiAlnum(prev) {
			b.WriteRune('\u200b')
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func asciiAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// restoreMentions undoes defuseMentions in what claude echoes back.
func restoreMentions(s string) string { return strings.ReplaceAll(s, "\u200b@", "@") }

const mentionNote = "\n\n(so inserted an invisible zero-width space before each @ that starts a word in the request, to stop file mentions from being expanded. Read past it; it is not part of the text.)"
