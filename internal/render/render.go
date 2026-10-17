// Package render formats panel results for the caller.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/peter-olom/second-opinion/internal/config"
	"github.com/peter-olom/second-opinion/internal/panel"
)

func JSON(w io.Writer, r panel.Result) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func Markdown(w io.Writer, r panel.Result) error {
	var b strings.Builder
	ok := len(r.Succeeded())

	b.WriteString("# Second opinion\n\n")
	if r.Run != "" {
		fmt.Fprintf(&b, "run `%s` · ", r.Run)
	}
	if r.Profile != "" {
		fmt.Fprintf(&b, "profile `%s` · ", r.Profile)
	}
	fmt.Fprintf(&b, "mode `%s` · %d of %d panelists responded\n", r.Mode, ok, len(r.Opinions))
	if r.Attached != "" {
		fmt.Fprintf(&b, "attached: %s\n", r.Attached)
	}
	if r.Workspace != "" {
		fmt.Fprintf(&b, "read-only repo access: `%s`\n", r.Workspace)
	}

	level := "##"
	if r.Synthesis != nil || r.ChairSkipped != "" {
		b.WriteString("\n---\n\n")
		if r.Synthesis != nil {
			fmt.Fprintf(&b, "## Panel synthesis (%s, %s)\n\n", r.Synthesis.Label, status(*r.Synthesis))
			writeBody(&b, *r.Synthesis)
		} else {
			fmt.Fprintf(&b, "## Panel synthesis\n\n_Skipped: %s._\n", r.ChairSkipped)
		}
		b.WriteString("\n---\n\n## Individual opinions\n")
		level = "###"
	}

	for _, o := range r.Opinions {
		b.WriteString("\n---\n\n")
		fmt.Fprintf(&b, "%s %s (%s)\n\n", level, o.Label, status(o))
		writeBody(&b, o)
	}

	// Named panels still need a key when a fallback answered: the heading
	// names the model that was asked, not the one that answered.
	if r.Anonymous || fellBack(r) {
		writeKey(&b, r)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func fellBack(r panel.Result) bool {
	for _, o := range r.Opinions {
		if len(o.Attempts) > 0 {
			return true
		}
	}
	return r.Synthesis != nil && len(r.Synthesis.Attempts) > 0
}

// writeKey maps anonymous labels to models, if the identities were not redacted.
func writeKey(b *strings.Builder, r panel.Result) {
	all := r.Opinions
	if r.Synthesis != nil {
		all = append(all[:len(all):len(all)], *r.Synthesis)
	}
	var lines []string
	for _, o := range all {
		if o.Panelist == nil {
			continue
		}
		line := fmt.Sprintf("- %s: %s", o.Label, keySpec(*o.Panelist))
		for _, a := range o.Attempts {
			if a.Panelist != nil {
				line += fmt.Sprintf(" (after %s hit a %s limit)", keySpec(*a.Panelist), a.Kind)
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n---\n\n## Panel key\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
}

// keySpec names a panelist in the key. Without a model, the CLI used its own
// default, which so cannot see, so the key says so rather than imply more.
func keySpec(p config.Panelist) string {
	if p.Model == "" {
		return fmt.Sprintf("`%s` (the CLI's default model, not recorded)", p.Display())
	}
	return fmt.Sprintf("`%s`", p.Display())
}

// status is the parenthetical after a heading: timing, any fallback, and why it failed.
func status(o panel.Opinion) string {
	s := o.Duration.String()
	if !o.OK() {
		s = fmt.Sprintf("failed: %s, after %s", o.ErrorKind, o.Duration)
	}
	if n := len(o.Attempts); n > 0 {
		s += fmt.Sprintf(", answered by fallback after %d usage limit%s", n, plural(n))
		if !o.OK() {
			s = fmt.Sprintf("failed: %s after %d fallback%s, %s", o.ErrorKind, n, plural(n), o.Duration)
		}
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func writeBody(b *strings.Builder, o panel.Opinion) {
	if o.OK() {
		b.WriteString(demoteHeadings(strings.TrimSpace(cleanText(o.Output))))
		b.WriteString("\n")
		return
	}
	fmt.Fprintf(b, "```\n%s\n```\n", strings.TrimSpace(cleanText(o.Error)))
	if hint := o.ErrorKind.Hint(); hint != "" {
		fmt.Fprintf(b, "\n%s\n", hint)
	}
}

// demoteHeadings moves a model's markdown headings two levels down, so an
// answer's "## Verdict" sits under so's own "## Panelist A" or "### Panelist
// A", and an answer can't pass off a "## Panel synthesis" or "## Panel key"
// of its own as one of so's sections. Fenced code blocks are left alone.
func demoteHeadings(s string) string {
	lines := strings.Split(s, "\n")
	fence := ""
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		switch {
		case fence != "":
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
		case strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"):
			fence = t[:3]
		case len(l)-len(t) <= 3 && strings.HasPrefix(t, "#"):
			lines[i] = "##" + t
		}
	}
	return strings.Join(lines, "\n")
}

// cleanText drops control characters other than newline and tab, so model
// output or a CLI's error can't move the cursor or rewrite the terminal
// (escape sequences, carriage returns, C1 controls).
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}
