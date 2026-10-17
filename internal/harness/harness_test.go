package harness

import "testing"

func TestContains(t *testing.T) {
	for path, want := range map[string]bool{
		".recallstack/s.json":               true,
		"packages/app/.specstory/history/x": true,
		".aider.chat.history.md":            true,
		"docs/.aider.input.history":         true,
		"src/recallstack.go":                false,
		"a/.specstory-notes/x":              false,
	} {
		if got := Contains(path); got != want {
			t.Errorf("Contains(%q) = %v, want %v", path, got, want)
		}
	}
}
