package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter-olom/second-opinion/internal/config"
	"github.com/peter-olom/second-opinion/internal/panel"
)

func record(t time.Time) Record {
	return Record{
		ID:     NewID(t),
		Time:   t,
		Prompt: "review this",
		Result: panel.Result{
			Mode:      "review",
			Anonymous: true,
			Opinions: []panel.Opinion{{
				Label:    "Panelist A",
				Panelist: &config.Panelist{Backend: "codex", Model: "gpt"},
				Output:   "fine",
				Seconds:  12.3,
			}},
		},
	}
}

func TestSaveLoadKeepsIdentities(t *testing.T) {
	dir := t.TempDir()
	r := record(time.Now())
	if err := Save(dir, r, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "last")
	if err != nil {
		t.Fatal(err)
	}
	op := got.Result.Opinions[0]
	if op.Panelist == nil || op.Panelist.Model != "gpt" {
		t.Errorf("identity not stored: %+v", op.Panelist)
	}
	if op.Duration != 12300*time.Millisecond {
		t.Errorf("duration = %s, want restored from seconds", op.Duration)
	}
	info, err := os.Stat(filepath.Join(dir, r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("run file permissions = %o, want 600 (it holds prompts and code)", perm)
	}
}

func TestLoadByPartialID(t *testing.T) {
	dir := t.TempDir()
	a := record(time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local))
	a.ID = "20261001-090000-aaaa"
	b := record(time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local))
	b.ID = "20261002-090000-bbbb"
	for _, r := range []Record{a, b} {
		if err := Save(dir, r, 100*365*24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	for ref, want := range map[string]string{"last": b.ID, "aaaa": a.ID, "20261002": b.ID, a.ID: a.ID} {
		got, err := Load(dir, ref)
		if err != nil || got.ID != want {
			t.Errorf("Load(%q) = %q, %v; want %q", ref, got.ID, err, want)
		}
	}
	if _, err := Load(dir, "2026100"); err == nil || !strings.Contains(err.Error(), "matches 2 runs") {
		t.Errorf("ambiguous ref: err = %v", err)
	}
	if _, err := Load(dir, "zzzz"); err == nil {
		t.Error("unknown ref should fail")
	}
	recs, err := List(dir, 0)
	if err != nil || len(recs) != 2 || recs[0].ID != b.ID {
		t.Errorf("List = %d records (first %q), %v; want newest first", len(recs), recs[0].ID, err)
	}
}

func TestSavePrunesOldRuns(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := record(now.Add(-8 * 24 * time.Hour))
	recent := record(now.Add(-6 * 24 * time.Hour))
	for _, r := range []Record{old, recent} {
		if err := Save(dir, r, 100*365*24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	// Saving a new run prunes anything older than the retention period.
	if err := Save(dir, record(now), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	recs, _ := List(dir, 0)
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	if len(recs) != 2 || slicesContains(ids, old.ID) || !slicesContains(ids, recent.ID) {
		t.Errorf("after prune: %v; want the 8-day-old run gone and the 6-day-old run kept", ids)
	}
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestStateDirIgnoresRelativeXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, x := range []string{".", "state", "~/state"} {
		t.Setenv("XDG_STATE_HOME", x)
		if got, want := StateDir(), filepath.Join(home, ".local/state/second-opinion"); got != want {
			t.Errorf("XDG_STATE_HOME=%q: StateDir() = %q, want %q", x, got, want)
		}
	}
	t.Setenv("XDG_STATE_HOME", "/srv/state")
	if got := Dir(); got != "/srv/state/second-opinion/runs" {
		t.Errorf("absolute XDG_STATE_HOME: Dir() = %q", got)
	}
}

func TestSaveNeverOverwritesOrFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	r := record(time.Now())
	if err := Save(dir, r, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, r, time.Hour); err == nil {
		t.Error("saving the same ID twice should fail rather than overwrite")
	}
	// A symlink planted at the next record's path must not be followed.
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, []byte("keep"), 0o644)
	r2 := record(time.Now())
	if err := os.Symlink(target, filepath.Join(dir, r2.ID+".json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := Save(dir, r2, time.Hour); err == nil {
		t.Error("Save followed a symlink")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Error("the symlink target was overwritten")
	}
}

func TestSaveTightensExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	os.MkdirAll(dir, 0o755)
	if err := Save(dir, record(time.Now()), time.Hour); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("history dir mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestIDsAreLongEnough(t *testing.T) {
	id := NewID(time.Now())
	if parts := strings.Split(id, "-"); len(parts) != 3 || len(parts[2]) != 8 {
		t.Errorf("NewID = %q, want 8 hex characters of randomness", id)
	}
}

func TestNonRunFilesAreNeverPrunedOrListed(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.json")
	os.WriteFile(notes, []byte("{}"), 0o644)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(notes, old, old)
	if err := Save(dir, record(time.Now()), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Error("a file that is not a run record was pruned")
	}
	recs, _ := List(dir, 0)
	if len(recs) != 1 {
		t.Errorf("List returned %d records, want only the real run", len(recs))
	}
}

func TestIDsUseUTC(t *testing.T) {
	zone := time.FixedZone("UTC+5", 5*3600)
	at := time.Date(2026, 10, 1, 3, 0, 0, 0, zone) // 2026-09-30 22:00 UTC
	if id := NewID(at); !strings.HasPrefix(id, "20260930-220000-") {
		t.Errorf("NewID = %q, want the UTC time", id)
	}
}
