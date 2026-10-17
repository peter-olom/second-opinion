package harness

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestContains(t *testing.T) {
	for path, want := range map[string]bool{
		".recallstack/s.json":               true,
		"packages/app/.specstory/history/x": true,
		".aider.chat.history.md":            true,
		"docs/.aider.input.history":         true,
		"src/recallstack.go":                false,
		"a/.specstory-notes/x":              false,
		".SPECSTORY/history/x.md":           true,
		"pkg/.RecallStack":                  true,
	} {
		if got := Contains(path); got != want {
			t.Errorf("Contains(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestFindSeesWhatGitWouldHide(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{
		"scratch/project/.recallstack",  // inside an ignored folder
		"vendor/sub/.SpecStory/history", // a submodule's work tree, other case
		"deep/a/b/c/.aider.chat.history.md",
		".git/.recallstack",           // never inside .git
		"node_modules/x/.recallstack", // nor node_modules
	} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	found, complete, err := Find(root, 10, 10000)
	if err != nil || !complete {
		t.Fatalf("Find: %v, complete %v", err, complete)
	}
	for _, want := range []string{"scratch/project/.recallstack", "vendor/sub/.SpecStory", "deep/a/b/c/.aider.chat.history.md"} {
		if !slices.Contains(found, want) {
			t.Errorf("Find = %q, missing %s", found, want)
		}
	}
	for _, f := range found {
		if filepath.Dir(f) == ".git" || filepath.Base(filepath.Dir(filepath.Dir(f))) == "node_modules" {
			t.Errorf("Find descended where it shouldn't: %s", f)
		}
	}
	if _, complete, _ := Find(root, 10, 3); complete {
		t.Error("a walk cut short by the limit should say it is incomplete")
	}
}
