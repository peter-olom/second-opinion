package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/peter-olom/second-opinion/internal/history"
)

// Seams for tests.
var (
	executable = os.Executable
	invokedAs  = func() string { return os.Args[0] }
	goos       = runtime.GOOS
)

// removal is one thing uninstall removes. Only files so created are deleted,
// and directories are removed only if that leaves them empty, so anything a
// user added (or a config that shares a directory) survives.
type removal struct {
	what  string
	path  string   // shown to the user
	files []string // deleted
	dirs  []string // then removed if empty, innermost first
	// rename moves the file aside instead of deleting it (a running Windows exe).
	rename string
	note   string
}

func cmdUninstall(ctx context.Context, args []string) int {
	flags, cfgPath := newFlagSet("uninstall", "so uninstall [--all] [--yes]\n\n"+
		"Removes the skill files written by `so skill install`, the run history, and the so binary.\n"+
		"Your config file (profiles and mode overrides) is kept unless you pass --all.")
	all := flags.Bool("all", false, "also remove the config file in use (--config or $SECOND_OPINION_CONFIG)")
	yes := flags.BoolP("yes", "y", false, "do not ask for confirmation")
	if code, ok := parse(flags, args); !ok {
		return code
	}
	if insidePanelist() {
		fmt.Fprintln(os.Stderr, "so: refusing to uninstall from inside a second-opinion panelist.")
		return 3
	}

	cfg, cfgParentIsLink, err := resolveConfigPath(*cfgPath)
	if err != nil {
		return fail(fmt.Errorf("%w; nothing was removed", err))
	}
	plan, notes, err := planUninstall(cfg, cfgParentIsLink, *all)
	if err != nil {
		return fail(fmt.Errorf("%w; nothing was removed", err))
	}
	for _, n := range notes {
		fmt.Println("note:", n)
	}
	if len(plan) == 0 {
		fmt.Println("nothing to remove")
		return 0
	}

	fmt.Println("This removes:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range plan {
		line := fmt.Sprintf("  %s\t%s", r.what, r.path)
		if r.note != "" {
			line += "  (" + r.note + ")"
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	if _, err := os.Stat(cfg); err == nil && !*all {
		fmt.Printf("Your config stays at %s (use --all to remove it too).\n", cfg)
	}
	if !*yes {
		fmt.Print("Continue? [y/N] ")
		type answer struct {
			line string
			err  error
		}
		answers := make(chan answer, 1)
		in := os.Stdin
		go func() {
			line, err := bufio.NewReader(in).ReadString('\n')
			answers <- answer{line, err}
		}()
		select {
		case <-ctx.Done():
			fmt.Println("\ncancelled; nothing was removed")
			return 130
		case a := <-answers:
			if a.err != nil && a.line == "" {
				fmt.Println()
				return fail(errors.New("no confirmation received; re-run with --yes to remove these"))
			}
			if s := strings.ToLower(strings.TrimSpace(a.line)); s != "y" && s != "yes" {
				fmt.Println("cancelled; nothing was removed")
				return 1
			}
		}
	}
	if ctx.Err() != nil {
		fmt.Println("cancelled; nothing was removed")
		return 130
	}

	// Identify the running binary now, while it still exists, so the closing
	// PATH note never calls a kept binary "another so".
	var running os.FileInfo
	if exe, err := executable(); err == nil {
		running, _ = os.Stat(exe)
	}

	// From here on, Ctrl-C is deliberately not checked: stopping halfway
	// through would leave a partial uninstall that is harder to reason about
	// than finishing the few remaining removals.
	//
	// Phase 1 deletes files and moves the binary aside; phase 2 removes
	// folders that are now empty. Deleting everything first means a folder
	// shared by two entries (say, history and config) is judged only once.
	failed := make([]bool, len(plan))
	for i, r := range plan {
		for _, f := range r.files {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "so: could not remove %s: %v\n", f, err)
				failed[i] = true
			}
		}
		if r.rename != "" {
			if err := os.Rename(r.path, r.rename); err != nil {
				fmt.Fprintf(os.Stderr, "so: could not move %s aside: %v\n", r.path, err)
				failed[i] = true
			}
		}
	}
	for i, r := range plan {
		for _, d := range r.dirs {
			if err := removeIfEmpty(d); err != nil && !errors.Is(err, errNotEmpty) {
				fmt.Fprintf(os.Stderr, "so: could not remove %s: %v\n", d, err)
				failed[i] = true
			}
		}
	}

	code := 0
	binaryRemoved, binaryPlanned := false, false
	for i, r := range plan {
		_, stillThere, _ := lstat(r.path)
		if r.what == "binary" || r.what == "binary link" {
			binaryPlanned = true
		}
		if r.what == "binary" && !failed[i] {
			binaryRemoved = true
		}
		switch {
		case failed[i]:
			code = 1
		case r.rename != "":
			fmt.Printf("moved %s to %s; delete it once this command exits\n", r.path, r.rename)
		case stillThere:
			why := "it still contains other files"
			if entries, err := os.ReadDir(r.path); err == nil && len(entries) == 0 {
				why = "a symlink may lead to it"
			}
			fmt.Printf("removed so's files from %s; kept the folder because %s\n", r.path, why)
		default:
			fmt.Println("removed", r.path)
		}
	}

	if other, err := lookPath("so"); err == nil && !plannedBinary(plan, other) {
		fi, err := os.Stat(other)
		isRunning := err == nil && running != nil && os.SameFile(fi, running)
		switch {
		case isRunning && !binaryPlanned:
			// The running binary, deliberately kept; its note was printed above.
		case isRunning && !binaryRemoved:
			fmt.Printf("note: so is still on your PATH at %s (the binary the removed link pointed to)\n", other)
		default:
			fmt.Printf("note: another so is still on your PATH at %s\n", other)
		}
	}
	fmt.Println("Skills installed elsewhere with `so skill install --dir` are not tracked; remove those yourself.")
	if code != 0 {
		fmt.Fprintln(os.Stderr, "so: some items could not be removed (see above)")
	}
	return code
}

// planUninstall works out what to remove without touching anything. Any error
// reading the filesystem aborts the whole uninstall before it deletes anything.
// cfgParentIsLink means the config's folder might be reached through a
// symlink; that folder is then never removed, even if deleting the config
// empties it.
func planUninstall(cfg string, cfgParentIsLink, all bool) (plan []removal, notes []string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	cfgInfo, cfgExists, err := lstat(cfg)
	if err != nil {
		return nil, nil, err
	}
	if cfgExists && !all {
		// Compare the file itself, not just the path, in case of symlinks.
		if fi, err := os.Stat(cfg); err == nil {
			cfgInfo = fi
		}
	}
	// keep reports whether a path must survive: the config, unless --all.
	keep := func(p string) bool {
		if all || !cfgExists {
			return false
		}
		if p == cfg {
			return true
		}
		fi, err := os.Stat(p)
		return err == nil && os.SameFile(fi, cfgInfo)
	}

	// Folders that a symlink we leave alone points to must not be removed,
	// or that link would dangle. The config's folder joins them below.
	var protected []os.FileInfo
	protectTarget := func(dir string) {
		if fi, ok, _ := lstat(dir); ok && fi.Mode()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(dir); err == nil {
				protected = append(protected, target)
			}
		}
	}

	for _, s := range skillDirs {
		dir := filepath.Join(home, s.dir, "second-opinion")
		protectTarget(dir)
		r, note, err := ownedDir(dir, []string{"SKILL.md"}, keep)
		if err != nil {
			return nil, nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
		if r != nil {
			r.what = s.harness + " skill"
			plan = append(plan, *r)
		}
	}

	if state := history.StateDir(); state != "" {
		protectTarget(state)
		runs := filepath.Join(state, "runs")
		protectTarget(runs)
		var names []string
		fi, ok, err := lstat(runs)
		if err != nil {
			return nil, nil, err
		}
		if ok && fi.IsDir() {
			entries, err := os.ReadDir(runs)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") && e.Type().IsRegular() {
					names = append(names, filepath.Join("runs", e.Name()))
				}
			}
		}
		r, note, err := ownedDir(state, names, keep, "runs")
		if err != nil {
			return nil, nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
		if r != nil {
			r.what = "run history"
			r.note = fmt.Sprintf("%d run%s", len(r.files), plural(len(r.files)))
			plan = append(plan, *r)
		}
	}

	if all && cfgExists {
		if cfgInfo.IsDir() {
			return nil, nil, fmt.Errorf("config path %s is a directory", cfg)
		}
		r := removal{what: "config", path: cfg, files: []string{cfg}}
		if dir := filepath.Dir(cfg); !cfgParentIsLink && filepath.Base(dir) == "second-opinion" {
			if dfi, ok, err := lstat(dir); err != nil {
				return nil, nil, err
			} else if ok && dfi.IsDir() { // never a symlinked directory
				if _, err := os.ReadDir(dir); err != nil {
					return nil, nil, err
				}
				r.dirs = []string{dir}
			}
		}
		plan = append(plan, r)
	}

	bins, note, err := binaryRemoval()
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		notes = append(notes, note)
	}
	plan = append(plan, bins...)

	// A config folder reached through a symlink is never removed either,
	// whichever entry would otherwise remove it (say, history sharing it).
	if cfgParentIsLink {
		if fi, err := os.Stat(filepath.Dir(cfg)); err == nil {
			protected = append(protected, fi)
		}
	}
	for i := range plan {
		dirs := plan[i].dirs[:0]
		for _, d := range plan[i].dirs {
			if !isProtected(d, protected) {
				dirs = append(dirs, d)
			}
		}
		plan[i].dirs = dirs
	}

	// Drop folders that hold nothing of so's: no files to delete, and they
	// would not end up empty. Listing them would promise a removal that
	// cannot happen.
	deleting, removable := map[string]bool{}, map[string]bool{}
	for _, r := range plan {
		for _, f := range r.files {
			deleting[f] = true
		}
		for _, d := range r.dirs {
			removable[d] = true
		}
	}
	kept := plan[:0]
	for _, r := range plan {
		if len(r.files) == 0 && r.rename == "" && !emptyAfter(r.path, deleting, removable) {
			// Still clear out any of so's own folders inside it that are empty.
			var empty []string
			for _, d := range r.dirs {
				if d != r.path && emptyAfter(d, deleting, removable) {
					empty = append(empty, d)
				}
			}
			if len(empty) == 0 {
				notes = append(notes, fmt.Sprintf("left %s alone: there is nothing of so's to remove", r.path))
				continue
			}
			r.path, r.dirs, r.note = empty[0], empty, "empty"
		}
		kept = append(kept, r)
	}
	return kept, notes, nil
}

func isProtected(dir string, protected []os.FileInfo) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for _, p := range protected {
		if os.SameFile(fi, p) {
			return true
		}
	}
	return false
}

// emptyAfter reports whether dir would be empty once the planned files are
// deleted and the planned folders inside it are removed.
func emptyAfter(dir string, deleting, removable map[string]bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if deleting[p] || (removable[p] && emptyAfter(p, deleting, removable)) {
			continue
		}
		return false
	}
	return true
}

// resolveConfigPath makes the config path absolute the way the filesystem
// does. filepath.Abs cleans "link/.." by text, but the kernel follows the link
// first, so only resolving the parent directory finds the file that other
// commands actually load. The last element is kept as is, so a config that is
// itself a symlink is removed as a link. It also reports whether the config's
// own folder is a symlink.
func resolveConfigPath(raw string) (path string, parentIsLink bool, err error) {
	if runtime.GOOS == "windows" {
		// Windows resolves rooted (\x), drive-relative (C:x) and ".." paths by
		// text before following reparse points, exactly as GetFullPathName
		// (filepath.Abs) does, so that is the path the OS opens.
		if raw, err = filepath.Abs(raw); err != nil {
			return "", false, err
		}
	}
	// filepath.Split keeps the separator and does not clean, so "link/../x"
	// keeps its "..".
	dir, base := filepath.Split(raw)
	if base == "" || base == "." || base == ".." {
		return "", false, fmt.Errorf("config path %q does not name a file", raw)
	}
	if dir, err = anchor(dir); err != nil {
		return "", false, fmt.Errorf("cannot resolve config path %s: %w", raw, err)
	}
	// Removing the config's folder is only a tidy-up, so fail safe: treat the
	// folder as a possible symlink unless the path names it plainly. A
	// relative path, or one with "." or ".." elements, can reach a symlinked
	// folder in ways a single Lstat cannot see.
	if !filepath.IsAbs(raw) || hasDotElem(dir) {
		parentIsLink = true
	} else if trimmed := strings.TrimRight(dir, `/`+string(filepath.Separator)); trimmed != "" && !strings.HasSuffix(trimmed, ":") {
		// Lstat follows every component but the last, so this asks whether
		// the config's own folder is a symlink.
		if fi, err := os.Lstat(trimmed); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			parentIsLink = true
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// A missing folder means the config is simply absent. Cleaning the
		// path as text is then harmless, unless it contains "..", where text
		// and the filesystem can disagree about which folder is meant.
		if !errors.Is(err, os.ErrNotExist) || hasDotDot(dir) {
			return "", false, fmt.Errorf("cannot resolve config path %s: %w", raw, err)
		}
		resolved = dir
	}
	path, err = filepath.Abs(filepath.Join(resolved, base))
	return path, parentIsLink, err
}

func hasDotDot(p string) bool { return hasElem(p, "..") }

func hasDotElem(p string) bool { return hasElem(p, ".") || hasElem(p, "..") }

func hasElem(p, want string) bool {
	for _, elem := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if elem == want {
			return true
		}
	}
	return false
}

// lookPath is exec.LookPath, but also accepts a match found through a relative
// PATH entry (exec.ErrDot). uninstall only inspects the path, never runs it.
func lookPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return "", err
	}
	return filepath.Abs(p)
}

// ownedDir plans removing the named files (relative to dir) and then dir
// itself and any listed subdirectories, if they end up empty. A dir that is a
// symlink or not a directory is left alone.
func ownedDir(dir string, files []string, keep func(string) bool, subdirs ...string) (*removal, string, error) {
	fi, ok, err := lstat(dir)
	if err != nil || !ok {
		return nil, "", err
	}
	if !fi.IsDir() {
		return nil, fmt.Sprintf("left %s alone: it is a symlink or not a directory", dir), nil
	}
	r := &removal{path: dir}
	for _, f := range files {
		p := filepath.Join(dir, f)
		ffi, ok, err := lstat(p)
		if err != nil {
			return nil, "", err
		}
		if ok && !ffi.IsDir() && !keep(p) {
			r.files = append(r.files, p)
		}
	}
	for _, sub := range subdirs {
		p := filepath.Join(dir, sub)
		if sfi, ok, err := lstat(p); err != nil {
			return nil, "", err
		} else if ok && sfi.IsDir() {
			r.dirs = append(r.dirs, p)
		}
	}
	r.dirs = append(r.dirs, dir)
	// Make sure every folder can be read now, so an unreadable one stops the
	// uninstall before anything is deleted rather than halfway through.
	for _, d := range r.dirs {
		if _, err := os.ReadDir(d); err != nil {
			return nil, "", err
		}
	}
	return r, "", nil
}

// binaryRemoval plans removing the so binary. It works out how so was started
// the way the filesystem does, and only removes something it can prove is the
// running binary: the file itself, or a symlink to it, in which case only the
// link goes. Anything else is left in place with a note.
func binaryRemoval() ([]removal, string, error) {
	raw, err := executable()
	if err != nil {
		return nil, "could not locate the running so binary; remove it yourself", nil
	}
	exe := raw
	if resolved, err := filepath.EvalSymlinks(raw); err == nil {
		exe = resolved
	}
	exeInfo, err := os.Stat(exe)
	if err != nil {
		return nil, fmt.Sprintf("could not inspect the running binary %s; remove it yourself", exe), nil
	}
	kept := func(why string) ([]removal, string, error) {
		return nil, fmt.Sprintf("the running binary %s was not removed because %s", exe, why), nil
	}

	if goos == "windows" {
		// If Windows reports so as started through a symlink, remove only the
		// link: it is not locked, and the program it points to stays intact.
		if fi, err := os.Lstat(raw); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return []removal{{what: "binary link", path: raw, files: []string{raw},
				note: "points to " + exe + ", which is kept"}}, "", nil
		}
		// A running exe cannot be deleted on Windows, but it can be renamed.
		// Never overwrite an existing file in the process.
		if !isSoName(exe) {
			return kept("it is not named so")
		}
		dest := exe + ".old"
		for n := 1; ; n++ {
			_, taken, err := lstat(dest)
			if err != nil {
				return nil, "", err
			}
			if !taken {
				break
			}
			dest = fmt.Sprintf("%s.old.%d", exe, n)
		}
		return []removal{{what: "binary", path: exe, rename: dest,
			note: "moved aside; Windows cannot delete a running program"}}, "", nil
	}

	started, err := startedFrom()
	if err != nil {
		return kept("it could not tell how so was started; remove it yourself")
	}
	fi, err := os.Lstat(started)
	if err != nil {
		return kept("it could not inspect " + started + "; remove it yourself")
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if ti, err := os.Stat(started); err == nil && os.SameFile(ti, exeInfo) {
			return []removal{{what: "binary link", path: started, files: []string{started},
				note: "points to " + exe + ", which is kept"}}, "", nil
		}
		return kept("so was started through " + started + ", which does not point to it")
	}
	if !os.SameFile(fi, exeInfo) {
		return kept("so was started from " + started + ", which is a different file")
	}
	if !isSoName(started) {
		return kept("it is not named so")
	}
	plan := []removal{{what: "binary", path: started, files: []string{started}}}
	// Links named so on PATH that point to this binary would dangle once it
	// is deleted, so they go too.
	for _, link := range linksOnPath(started) {
		plan = append(plan, removal{what: "binary link", path: link, files: []string{link},
			note: "points to the binary being removed"})
	}
	return plan, "", nil
}

// linksOnPath returns symlinks named so in PATH folders that resolve to the
// binary at bin (a physical path) itself, not merely to another name for the
// same file such as a hard link, which uninstall keeps.
func linksOnPath(bin string) []string {
	var links []string
	seen := map[string]bool{bin: true}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p, err := physicalPath(dir + string(filepath.Separator) + "so")
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved == bin {
			links = append(links, p)
		}
	}
	return links
}

// startedFrom returns the path so was started from, resolved the way the
// filesystem resolves it: through symlinks first, then "..".
func startedFrom() (string, error) {
	arg := invokedAs()
	if !strings.ContainsAny(arg, `/`+string(filepath.Separator)) {
		p, err := searchPath(arg)
		if err != nil {
			return "", err
		}
		arg = p
	}
	return physicalPath(arg)
}

// searchPath finds name on $PATH the way a shell does, returning the match as
// the PATH entry joined to name by concatenation, so ".." in an entry is not
// cleaned away as text.
func searchPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p := dir + string(filepath.Separator) + name
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// physicalPath resolves raw's folder through symlinks the way the kernel does
// (a relative path from the physical working directory) and keeps the last
// element as is, so a symlink at the end stays a symlink.
func physicalPath(raw string) (string, error) {
	dir, base := filepath.Split(raw)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("%q does not name a file", raw)
	}
	dir, err := anchor(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}

// anchor makes a relative folder absolute against the physical working
// directory. os.Getwd may return a logical $PWD through a symlink, against
// which ".." would be resolved differently than the kernel does. It
// concatenates rather than joins, because Join would clean "..".
func anchor(dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return dir, nil
	}
	wd, err := os.Getwd()
	if err == nil {
		wd, err = filepath.EvalSymlinks(wd)
	}
	if err != nil {
		return "", err
	}
	return wd + string(filepath.Separator) + dir, nil
}

func isSoName(p string) bool {
	name := filepath.Base(p)
	return name == "so" || name == "so.exe"
}

// plannedBinary reports whether p is the binary (or link) this run tried to
// remove, comparing both the path and, for the binary, where it resolves to.
func plannedBinary(plan []removal, p string) bool {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		resolved = p
	}
	for _, r := range plan {
		switch {
		case r.what == "binary link" && r.path == p:
			return true
		case r.what == "binary" && (r.path == p || r.path == resolved):
			return true
		}
	}
	return false
}

var errNotEmpty = errors.New("directory not empty")

func removeIfEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errNotEmpty
	}
	return os.Remove(dir)
}

// lstat reports whether p exists, treating only "does not exist" as absent.
func lstat(p string) (os.FileInfo, bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return fi, true, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
