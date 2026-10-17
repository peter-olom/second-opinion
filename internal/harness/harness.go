// Package harness names the files and folders where agent harnesses keep
// session state (conversation history, summaries, transcript paths) inside
// a repository. They aren't part of the code under review: --diff never
// attaches them, claude panelists are denied them, and so warns that codex
// and pi panelists with repo access can read them.
package harness

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

// Names are matched against every component of a path.
var Names = []string{".recallstack", ".specstory", ".aider.chat.history.md", ".aider.input.history"}

// Contains reports whether a slash-separated relative path is, or is
// inside, harness state at any depth. Case is ignored: on a
// case-insensitive filesystem a harness writing .specstory/ fills a
// committed .SPECSTORY/.
func Contains(path string) bool {
	for _, part := range strings.Split(path, "/") {
		for _, n := range Names {
			if strings.EqualFold(part, n) {
				return true
			}
		}
	}
	return false
}

// Find walks root for harness state, at any depth and in any letter case:
// ignored folders, submodules, and nested repositories included, which a
// git listing would collapse or skip. It returns up to max paths, relative
// to root, and complete = false if it stopped early, after limit entries.
// It doesn't descend into .git folders, node_modules, or harness state
// itself, and doesn't follow symlinks.
func Find(root string, max, limit int) (found []string, complete bool, err error) {
	seen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if seen++; seen > limit {
			return errStop
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if Contains(name) {
			rel, _ := filepath.Rel(root, path)
			found = append(found, filepath.ToSlash(rel))
			if len(found) >= max {
				return errDone
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && (name == ".git" || name == "node_modules") {
			return filepath.SkipDir
		}
		return nil
	})
	switch {
	case errors.Is(err, errDone):
		return found, true, nil
	case errors.Is(err, errStop):
		return found, false, nil
	}
	return found, err == nil, err
}

var (
	errStop = errors.New("stopped")
	errDone = errors.New("done")
)
