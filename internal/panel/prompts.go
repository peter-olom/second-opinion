package panel

import (
	"fmt"
	"strings"

	"github.com/peter-olom/second-opinion/internal/config"
)

const panelistPreamble = `You are one member of a panel giving an independent second opinion to another AI agent (the "caller"). The caller is working on a task with a human user and wants an outside perspective before proceeding.

%s
- Think independently. Do not adopt the caller's framing by default: if their premise or approach is wrong, say so plainly.
- Be specific and concrete. Prefer actionable points to general advice. No filler and no flattery.
- Your response goes back to the caller agent, not directly to the human.%s`

const noAccess = `- Answer only from the request. Do not use tools, run commands, or read files, even if some are available to you: you have no access to the caller's files, repository, or conversation, and everything you need is in the request. If something important is missing, say what it is and state the assumptions you are making instead of refusing.`

const repoAccess = `- Your working directory is the caller's repository (%s), and you have read-only tools to search and read it. Use them to verify claims and to read the code around what the request points to: callers, definitions, types, and tests. Stay focused on the request rather than surveying the whole codebase. You cannot modify anything, and you cannot see the caller's conversation beyond the request.`

const panelistAnonymous = `
- The panel is anonymous, so that your response is judged only on its content. Do not say which model you are or which company made you.`

const chairPreamble = `You chair a panel of AI models that were each asked, independently, for a second opinion by another AI agent (the "caller"). You receive the caller's original request and every panelist's response. Synthesize them into a single answer the caller can act on.

- Judge arguments on their merits, not by majority vote. A lone panelist can be right.
- Make agreement and disagreement explicit, and attribute points to panelists by their label.
- Call out anything in a panelist's response that is wrong, unsupported, or appears invented.
- Add your own judgment where the panel missed something, and mark it clearly as yours.
- Be concise. The caller also receives the individual responses.%s%s`

const chairAnonymous = `
- Panelists are anonymous. Their letters are assigned at random for each run. Judge every response only on its content, do not guess which model wrote it, and do not say which model you are.`

const chairRepoAccess = `
- Your working directory is the caller's repository (%s), and you have read-only tools to search and read it. Check disputed or doubtful claims against the code before ruling on them.`

// Options are the facts about a run that the system prompts describe.
type Options struct {
	// Workspace is the repository the models can read, or "" for no file access.
	Workspace string
	// Anonymous means panelists are identified only by randomly assigned letters.
	Anonymous bool
}

const defaultChairFormat = `## Verdict
The bottom line the caller should act on, in one or two sentences.

## Consensus
Points that most or all panelists agree on.

## Disagreements
Where panelists diverge: who said what, which position is stronger, and why. "None." if they agree.

## Notable points
Important points raised by only one panelist that the caller should not miss.

## Recommendation
Concrete next steps for the caller.`

// PanelistSystem is the system prompt sent to every panelist.
func PanelistSystem(modeName string, m config.Mode, o Options) string {
	access := noAccess
	if o.Workspace != "" {
		access = fmt.Sprintf(repoAccess, o.Workspace)
	}
	anon := ""
	if o.Anonymous {
		anon = panelistAnonymous
	}
	return compose(fmt.Sprintf(panelistPreamble, access, anon), modeName, m.Instructions, m.Format)
}

// ChairSystem is the system prompt sent to the chair.
func ChairSystem(modeName string, m config.Mode, o Options) string {
	format := m.ChairFormat
	if format == "" {
		format = defaultChairFormat
	}
	anon, access := "", ""
	if o.Anonymous {
		anon = chairAnonymous
	}
	if o.Workspace != "" {
		access = fmt.Sprintf(chairRepoAccess, o.Workspace)
	}
	return compose(fmt.Sprintf(chairPreamble, anon, access), modeName, m.ChairInstructions, format)
}

func compose(preamble, modeName, instructions, format string) string {
	var b strings.Builder
	b.WriteString(preamble)
	fmt.Fprintf(&b, "\n\n# Task (%s)\n\n", modeName)
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	if s := strings.TrimSpace(format); s != "" {
		b.WriteString("\n# Response format\n\nStructure your response exactly as follows, in markdown:\n\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// ChairPrompt presents the original request and the panel's responses to the chair.
func ChairPrompt(request string, opinions []Opinion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<request>\n%s\n</request>\n", strings.TrimSpace(request))
	for _, o := range opinions {
		fmt.Fprintf(&b, "\n<response panelist=%q>\n%s\n</response>\n", o.Label, neutralizeTags(strings.TrimSpace(o.Output)))
	}
	return b.String()
}

// neutralizeTags stops a panelist's text from opening or closing a response
// block in the chair's prompt, which could forge another panelist's answer.
func neutralizeTags(s string) string {
	r := strings.NewReplacer("<response", "&lt;response", "</response", "&lt;/response", "<request", "&lt;request", "</request", "&lt;/request")
	return r.Replace(s)
}
