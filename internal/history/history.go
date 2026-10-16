// Package history keeps a private local record of every run, including which
// model was which panelist, so an anonymous panel can be unblinded later.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/peter-olom/second-opinion/internal/panel"
)

// Record is everything needed to inspect a run afterwards.
type Record struct {
	ID             string       `json:"id"`
	Time           time.Time    `json:"time"`
	Dir            string       `json:"dir"` // the caller's working directory
	Prompt         string       `json:"prompt"`
	PanelistSystem string       `json:"panelist_system"`
	ChairSystem    string       `json:"chair_system,omitempty"`
	Result         panel.Result `json:"result"` // with model identities
}

// StateDir is so's state directory: $XDG_STATE_HOME/second-opinion, or
// ~/.local/state/second-opinion. A relative XDG_STATE_HOME is ignored, as the
// XDG spec requires. It is "" if no home directory can be found.
func StateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "second-opinion")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".local", "state", "second-opinion")
}

// Dir is where runs are stored: StateDir()/runs, or "" if there is no state directory.
func Dir() string {
	if s := StateDir(); s != "" {
		return filepath.Join(s, "runs")
	}
	return ""
}

const idTimeLayout = "20060102-150405"

// runFileName matches the names so gives run records; anything else in the
// runs folder is not so's, and is never listed, pruned, or uninstalled.
var runFileName = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{4,16}\.json$`)

// IsRunFile reports whether name is a run record so created.
func IsRunFile(name string) bool { return runFileName.MatchString(name) }

// NewID returns an ID that sorts by time, e.g. 20261015-143012-a3f9c2e1.
func NewID(t time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return t.Format(idTimeLayout) + "-" + hex.EncodeToString(b)
}

// Save writes the record privately, then prunes runs older than maxAge.
func Save(dir string, r Record, maxAge time.Duration) error {
	if dir == "" {
		return errors.New("no home directory for the run history")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone; records hold prompts
	// and code, so make sure only the user can read them.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	// O_EXCL never follows a symlink and never overwrites an existing record.
	f, err := os.OpenFile(filepath.Join(dir, r.ID+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	Prune(dir, time.Now().Add(-maxAge))
	return nil
}

// Prune deletes runs recorded before cutoff. A run's time comes from its ID,
// or the file's modification time if the ID does not parse.
func Prune(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !IsRunFile(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, err := time.ParseInLocation(idTimeLayout, id[:min(len(id), len(idTimeLayout))], time.Local)
		if err != nil {
			info, err := e.Info()
			if err != nil {
				continue
			}
			t = info.ModTime()
		}
		if t.Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// List returns stored runs, newest first, up to n (n <= 0 for all).
func List(dir string, n int) ([]Record, error) {
	ids, err := ids(dir)
	if err != nil {
		return nil, err
	}
	slices.Reverse(ids)
	if n > 0 && len(ids) > n {
		ids = ids[:n]
	}
	var recs []Record
	for _, id := range ids {
		r, err := read(dir, id)
		if err != nil {
			continue
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// Load returns the run matching ref: "last", a full ID, or any unique part of one.
func Load(dir, ref string) (Record, error) {
	ids, err := ids(dir)
	if err != nil {
		return Record{}, err
	}
	if len(ids) == 0 {
		return Record{}, errors.New("no runs recorded yet")
	}
	if ref == "last" {
		return read(dir, ids[len(ids)-1])
	}
	var matches []string
	for _, id := range ids {
		if id == ref {
			return read(dir, id)
		}
		if strings.Contains(id, ref) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, fmt.Errorf("no run matches %q (see `so runs`)", ref)
	case 1:
		return read(dir, matches[0])
	}
	return Record{}, fmt.Errorf("%q matches %d runs; use more of the ID", ref, len(matches))
}

func read(dir, id string) (Record, error) {
	var r Record
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("run %s: %w", id, err)
	}
	// Durations are stored as seconds.
	for i := range r.Result.Opinions {
		restoreDuration(&r.Result.Opinions[i])
	}
	if r.Result.Synthesis != nil {
		restoreDuration(r.Result.Synthesis)
	}
	return r, nil
}

func restoreDuration(o *panel.Opinion) {
	o.Duration = time.Duration(o.Seconds * float64(time.Second)).Round(100 * time.Millisecond)
}

// ids returns stored run IDs, oldest first.
func ids(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if IsRunFile(e.Name()) && e.Type().IsRegular() {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	slices.Sort(ids)
	return ids, nil
}
