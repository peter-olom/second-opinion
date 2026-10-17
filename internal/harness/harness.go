// Package harness names the files and folders where agent harnesses keep
// session state (conversation history, summaries, transcript paths) inside
// a repository. They aren't part of the code under review: --diff never
// attaches them, claude panelists are denied them, and so warns that codex
// and pi panelists with repo access can read them.
package harness

import "strings"

// Names are matched against every component of a path.
var Names = []string{".recallstack", ".specstory", ".aider.chat.history.md", ".aider.input.history"}

// Contains reports whether a slash-separated relative path is, or is
// inside, harness state at any depth.
func Contains(path string) bool {
	for _, part := range strings.Split(path, "/") {
		for _, n := range Names {
			if part == n {
				return true
			}
		}
	}
	return false
}
