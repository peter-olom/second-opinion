// Command so gets second opinions from other AI models through the agent CLIs you already use.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"

	"github.com/peter-olom/second-opinion/internal/assets"
	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/config"
	"github.com/peter-olom/second-opinion/internal/gitctx"
	"github.com/peter-olom/second-opinion/internal/harness"
	"github.com/peter-olom/second-opinion/internal/history"
	"github.com/peter-olom/second-opinion/internal/panel"
	"github.com/peter-olom/second-opinion/internal/procgroup"
	"github.com/peter-olom/second-opinion/internal/render"
)

var version = "dev"

const usage = `so: get second opinions from other AI models

Usage:
  so ask [flags] [prompt]   Ask a profile (one model or a panel) for a second opinion
  so profiles               List profiles
  so modes [name]           List modes, or show the prompts a mode produces
  so runs [show <id>]       List recorded runs, or show one (--reveal to see which model said what)
  so doctor [--ping]        Check backend CLIs, and optionally ping each configured model
  so init [--force]         Write the default config for editing
  so uninstall [--all]      Remove so, its skills, and its run history (--all: config too)
  so skill [install]        Print the agent skill, or install it for Claude Code, Codex, and pi
  so version

Run "so ask --help" for ask flags.
`

func main() {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) { // keep `nohup so ask …` working
		sigs = append(sigs, syscall.SIGHUP)
	}
	ctx, stop := signal.NotifyContext(context.Background(), sigs...)
	defer stop()
	// The first signal cancels gracefully (killing panelists' process groups);
	// restoring default handling means a second one exits immediately, even
	// if so is stuck reading input.
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ask":
		return cmdAsk(ctx, rest)
	case "profiles":
		return cmdProfiles(rest)
	case "modes":
		return cmdModes(rest)
	case "runs":
		return cmdRuns(rest)
	case "doctor":
		return cmdDoctor(ctx, rest)
	case "init":
		return cmdInit(rest)
	case "uninstall":
		return cmdUninstall(ctx, rest)
	case "skill":
		return cmdSkill(rest)
	case "version", "--version":
		fmt.Println("so", version)
		return 0
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "so: unknown command %q\n\n%s", cmd, usage)
	return 2
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "so:", err)
	return 1
}

// checkSpecs parses backend:model@effort flag values, so a typo is reported
// as a usage error before anything else runs.
func checkSpecs(specs ...string) error {
	for _, spec := range specs {
		if spec == "" {
			continue
		}
		pl, err := config.ParseSpec(spec)
		if err != nil {
			return err
		}
		if !backend.Known(pl.Backend) {
			return fmt.Errorf("unknown backend %q in %q (want one of %s)", pl.Backend, spec, strings.Join(backend.Names(), ", "))
		}
	}
	return nil
}

// usageErr reports a malformed command line.
func usageErr(err error) int {
	fmt.Fprintln(os.Stderr, "so:", err)
	return 2
}

// failCtx reports err, or an interruption as such (exit 130).
func failCtx(ctx context.Context, err error) int {
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "so: interrupted")
		return 130
	}
	return fail(err)
}

// parse parses flags, returning an exit code if the command should stop.
func parse(fs *pflag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

func newFlagSet(name, synopsis string) (*pflag.FlagSet, *string) {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s\n\nFlags:\n%s", synopsis, fs.FlagUsages())
	}
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	return fs, cfgPath
}

func cmdAsk(ctx context.Context, args []string) int {
	fs, cfgPath := newFlagSet("ask", "so ask [flags] [prompt]\n\n"+
		"The prompt is taken from the arguments, or from stdin if there are none.\n"+
		"End the arguments with - to attach stdin as well (e.g. git diff | so ask -m review \"...\" -).")
	profile := fs.StringP("profile", "p", "", "profile to ask (default from config)")
	mode := fs.StringP("mode", "m", "", "response mode: general, review, plan, debug, audit, or one from your config")
	files := fs.StringArrayP("file", "f", nil, "attach a file to the prompt (repeatable)")
	diff := fs.Bool("diff", false, "attach the git diff (against --base) and the full contents of changed and untracked (not ignored) files")
	base := fs.String("base", "", "base ref for --diff, e.g. main (default HEAD: uncommitted changes); implies --diff")
	repo := fs.Bool("repo", false, "give the panel read-only access to the current repository")
	noRepo := fs.Bool("no-repo", false, "no repository access, even if the mode enables it by default")
	with := fs.StringArrayP("with", "w", nil, "ad-hoc panelist backend:model@effort instead of a profile (repeatable)")
	chair := fs.String("chair", "", "chair for a multi-panelist run, as backend:model@effort")
	noChair := fs.Bool("no-chair", false, "skip the chair's synthesis")
	reveal := fs.Bool("reveal", false, "append a key showing which model was which panelist (the chair stays blind)")
	format := fs.String("format", "md", "output format: md or json")
	timeout := fs.Duration("timeout", 0, "per-model timeout (default from config)")
	quiet := fs.BoolP("quiet", "q", false, "no progress output")
	verbose := fs.BoolP("verbose", "v", false, "progress output even when stderr is not a terminal")
	if code, ok := parse(fs, args); !ok {
		return code
	}

	if insidePanelist() {
		fmt.Fprintln(os.Stderr, "so: refusing to run inside a second-opinion panelist; answer the request yourself.")
		return 3
	}
	if *timeout < 0 {
		return usageErr(errors.New("--timeout must be positive"))
	}
	if *format != "md" && *format != "json" {
		return usageErr(fmt.Errorf("unknown format %q (want md or json)", *format))
	}
	if *profile != "" && len(*with) > 0 {
		return usageErr(errors.New("use either --profile or --with, not both"))
	}
	if *repo && *noRepo {
		return usageErr(errors.New("use either --repo or --no-repo, not both"))
	}
	if *chair != "" && *noChair {
		return usageErr(errors.New("use either --chair or --no-chair, not both"))
	}
	if *quiet && *verbose {
		return usageErr(errors.New("use either --quiet or --verbose, not both"))
	}
	if err := checkSpecs(append(*with, *chair)...); err != nil {
		return usageErr(err)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	// Expired runs go now, so even a run that stops early prunes them.
	history.Prune(history.Dir(), time.Now().Add(-cfg.Retention()))
	p, err := resolvePanel(cfg, *profile, *mode, *with, *chair, *noChair)
	if err != nil {
		return fail(err)
	}
	if *chair != "" && len(p.Panelists) < 2 {
		return usageErr(errors.New("--chair needs at least two panelists to synthesize"))
	}
	// Attachment tags carry a random suffix so attached content can't close
	// its own block and pose as another attachment.
	nonce := newNonce()
	prompt, err := readPrompt(ctx, fs.Args(), *files, nonce)
	if err != nil {
		return failCtx(ctx, err)
	}
	var attached string
	if *diff || *base != "" {
		// Same rule as repo access: never collect from a repository rooted
		// at home or above (a dotfiles repo in ~), whose untracked files
		// could be anything in $HOME.
		if err := checkHome(); err != nil {
			return fail(fmt.Errorf("--diff: %w", err))
		}
		if root, err := gitctx.Root(ctx, "."); err == nil && containsHome(root) {
			return fail(errors.New("--diff would collect from a git repository rooted at your home directory; run so inside a project repository"))
		} else if err == nil && inHomeDotDir(root) && !*repo && !*noRepo {
			// Same rule as a mode's repo access: a repository in ~/.config
			// or the like has untracked tokens and configs that --diff
			// would attach. Either explicit choice confirms it.
			return fail(errors.New("--diff: this repository is inside a dot-folder in your home directory, where untracked files are often tokens or configs; to collect from it anyway, add --no-repo (the diff only) or --repo (the diff, and read access for the panel)"))
		}
		gc, err := gitctx.Diff(ctx, ".", firstNonEmpty(*base, "HEAD"), nonce)
		if err != nil {
			return failCtx(ctx, err)
		}
		prompt += "\n\n" + gc.Text
		attached = gc.Summary
	}
	if len(prompt) > maxPromptBytes {
		return fail(fmt.Errorf("the prompt with attachments is %d bytes, over the %d-byte limit; attach less", len(prompt), maxPromptBytes))
	}
	if p.Workspace, err = resolveWorkspace(ctx, *repo, *noRepo, p.Mode.Repo); err != nil {
		return failCtx(ctx, err)
	}
	if p.Workspace != "" {
		// Anywhere in the repository, not only at its root: a harness run
		// in a subfolder keeps its state there.
		found, err := gitctx.HarnessPaths(ctx, p.Workspace, 5)
		if err != nil && ctx.Err() == nil {
			found = nil
			for _, name := range harness.Names {
				if _, err := os.Lstat(filepath.Join(p.Workspace, name)); err == nil {
					found = append(found, name)
				}
			}
		}
		for _, name := range found {
			fmt.Fprintf(os.Stderr, "so: %s holds agent-harness state; claude panelists are denied it, but codex and pi panelists with repo access can read it (use --no-repo to prevent that)\n", render.Line(name))
		}
	}
	p.Anonymous = cfg.Anonymous()

	to := *timeout
	if to == 0 {
		to, _ = cfg.TimeoutDuration()
	}
	runner := &panel.Runner{Backends: cfg.Backends, Timeout: to}
	if !*quiet && (*verbose || isTerminal(os.Stderr)) {
		runner.Progress = os.Stderr
	}

	start := time.Now()
	// The ID is announced before the panel starts, so a caller running so in
	// the background can wait for that exact run with so runs show <id>.
	var id string
	recording := cfg.Recording()
	if recording {
		id = history.NewID(start)
		if err := history.Writable(history.Dir()); err != nil {
			fmt.Fprintln(os.Stderr, "so: this run won't be recorded:", err)
			recording = false
		} else if !*quiet {
			fmt.Fprintf(os.Stderr, "so: run %s\n", id)
		}
	}
	res := runner.Run(ctx, p, prompt)
	res.Attached = attached
	if recording {
		res.Run = id
		if err := saveRun(cfg, p, prompt, res, start); err != nil {
			fmt.Fprintln(os.Stderr, "so: could not record run:", err)
			res.Run = ""
		}
	} else {
		// Recording is off, but runs recorded before that still expire.
		history.Prune(history.Dir(), time.Now().Add(-cfg.Retention()))
	}
	if res.Anonymous && !*reveal {
		res.Redact()
	}
	if *format == "json" {
		err = render.JSON(os.Stdout, res)
	} else {
		err = render.Markdown(os.Stdout, res)
	}
	switch {
	case err != nil:
		return fail(err)
	case ctx.Err() != nil:
		return 130
	case len(res.Succeeded()) == 0:
		return 1
	}
	return 0
}

func resolvePanel(cfg *config.Config, profile, mode string, with []string, chair string, noChair bool) (panel.Panel, error) {
	var p panel.Panel
	var profileMode string
	if len(with) > 0 {
		for _, spec := range with {
			pl, err := config.ParseSpec(spec)
			if err != nil {
				return p, err
			}
			p.Panelists = append(p.Panelists, pl)
		}
		p.Chair = cfg.DefaultChair
	} else {
		name, pr, err := cfg.Profile(profile)
		if errors.Is(err, config.ErrNoProfiles) {
			return p, errNoProfiles(cfg)
		}
		if err != nil {
			return p, err
		}
		p.Profile, p.Panelists, p.Chair, profileMode = name, pr.Panelists, pr.Chair, pr.Mode
	}

	p.ModeName = firstNonEmpty(mode, profileMode, cfg.DefaultMode)
	m, ok := cfg.Modes[p.ModeName]
	if !ok {
		return p, fmt.Errorf("unknown mode %q (available: %s)", p.ModeName, strings.Join(cfg.ModeNames(), ", "))
	}
	p.Mode = m

	if chair != "" {
		c, err := config.ParseSpec(chair)
		if err != nil {
			return p, err
		}
		p.Chair = &c
	}
	if noChair || len(p.Panelists) < 2 {
		p.Chair = nil
	}
	return p, p.Validate()
}

func saveRun(cfg *config.Config, p panel.Panel, prompt string, res panel.Result, start time.Time) error {
	opts := panel.Options{Workspace: p.Workspace, Anonymous: p.Anonymous}
	rec := history.Record{
		ID:             res.Run,
		Time:           start,
		Prompt:         prompt,
		PanelistSystem: panel.PanelistSystem(p.ModeName, p.Mode, opts),
		Result:         res,
	}
	rec.Dir, _ = os.Getwd()
	if res.Synthesis != nil {
		rec.ChairSystem = panel.ChairSystem(p.ModeName, p.Mode, opts)
	}
	return history.Save(history.Dir(), rec, cfg.Retention())
}

// resolveWorkspace decides which repository, if any, the panel may read.
// An explicit --repo uses the enclosing git repository, or the current directory
// outside one. A mode's default only applies inside a git repository, and never
// to one rooted at the home directory or above it (say, a dotfiles repo in ~),
// so a panel never silently gets access to all of $HOME.
func resolveWorkspace(ctx context.Context, explicit, disabled bool, modeDefault *bool) (string, error) {
	if disabled || (!explicit && (modeDefault == nil || !*modeDefault)) {
		return "", nil
	}
	// Without a home directory, what repo access would expose can't be
	// checked; say so rather than blame a home-rooted repository.
	if err := checkHome(); err != nil {
		if explicit {
			return "", fmt.Errorf("--repo: %w", err)
		}
		fmt.Fprintf(os.Stderr, "so: no repo access for this mode: %v\n", err)
		return "", nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Use the physical path, as git does for the repository root.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	root, err := gitctx.Root(ctx, cwd)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err == nil {
		// A repository inside a dot-folder under home (~/.config, ~/.claude)
		// holds tokens, configs, or saved conversations; a mode never shares
		// one on its own.
		if !explicit && inHomeDotDir(root) {
			fmt.Fprintln(os.Stderr, "so: no repo access for this mode: the repository is inside a dot-folder in your home directory; pass --repo to share it")
			return "", nil
		}
		if !containsHome(root) {
			return root, nil
		}
		// A repository rooted at home or above (a dotfiles repo in ~) would
		// open up all of $HOME. A mode default never does that; an explicit
		// --repo gets the current folder instead, unless that is home itself.
		if !explicit {
			fmt.Fprintln(os.Stderr, "so: no repo access for this mode: the git repository is rooted at your home directory; pass --repo to share the current folder")
			return "", nil
		}
		if containsHome(cwd) {
			return "", errors.New("--repo would give the panel your whole home directory (it is inside a git repository rooted there); run so from a project folder instead")
		}
		return cwd, nil
	}
	if explicit {
		if containsHome(cwd) {
			return "", errors.New("--repo would give the panel your whole home directory; run so from a project folder instead")
		}
		return cwd, nil
	}
	if errors.Is(err, gitctx.ErrForeignWorkTree) {
		fmt.Fprintf(os.Stderr, "so: no repo access for this mode: %v\n", err)
	} else {
		fmt.Fprintln(os.Stderr, "so: no repo access for this mode: not inside a git repository; pass --repo to share the current folder")
	}
	return "", nil
}

// inHomeDotDir reports whether dir is inside a dot-folder directly under the
// home directory, such as ~/.config/nvim.
func inHomeDotDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return strings.HasPrefix(first, ".")
}

// checkHome reports whether the home directory can be resolved, which
// containsHome needs to give a real answer.
func checkHome() error {
	home, err := os.UserHomeDir()
	if err == nil {
		_, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		return fmt.Errorf("cannot resolve your home directory to check what would be exposed (%v); set HOME", err)
	}
	return nil
}

// containsHome reports whether dir is the home directory or one of its ancestors.
func containsHome(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true // cannot tell, so be careful
	}
	h, err1 := filepath.EvalSymlinks(home)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return true
	}
	rel, err := filepath.Rel(d, h)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// insidePanelist reports whether so is running inside one of its own
// panelists. The guard is cooperative: it stops accidental recursion, but a
// panelist with a shell could clear the variable.
func insidePanelist() bool {
	_, ok := os.LookupEnv(backend.EnvGuard)
	return ok
}

// Limits on what the caller can send, since all of it goes to every panelist,
// the chair, and the run history. Variables so tests can lower them.
var (
	maxStdinBytes  = 2 << 20
	maxFileBytes   = 1 << 20
	maxPromptBytes = 6 << 20
)

// readPrompt builds the prompt from arguments, stdin, and attached files.
// Stdin is read only when there is no prompt argument, or the argument is
// "-", so a so called inside a shell loop never swallows the loop's input.
func readPrompt(ctx context.Context, args, files []string, nonce string) (string, error) {
	// Decided from the arguments themselves, not their text, so an empty
	// argument (so ask "$q" with $q unset) never reads stdin.
	useStdin := len(args) == 0 || args[len(args)-1] == "-"
	explicitDash := len(args) > 0 && args[len(args)-1] == "-"
	if explicitDash {
		args = args[:len(args)-1]
	}
	if explicitDash && !stdinHasData() {
		// An explicit "-" asks for stdin. If it is a socket or terminal it is
		// not read (a harness socket that never closes would hang so), and
		// silently answering without the data would be worse than an error.
		return "", errors.New(`"-" asks for stdin, but stdin is not a pipe or file; pipe the data in, or write it to a file and use -f`)
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if !useStdin && stdinIsFile() {
		fmt.Fprintln(os.Stderr, "so: stdin was not read; end the arguments with - to attach it")
	}
	if useStdin && stdinHasData() {
		data, err := readStdin(ctx)
		if err != nil {
			return "", err
		}
		if len(data) > maxStdinBytes {
			return "", fmt.Errorf("stdin is larger than %d bytes; send a smaller excerpt", maxStdinBytes)
		}
		if in := strings.TrimSpace(string(data)); in != "" {
			if text == "" {
				text = in
			} else {
				tag := gitctx.Tag("stdin", nonce)
				text += "\n\n<" + tag + ">\n" + in + "\n</" + tag + ">"
			}
		}
	}
	var b strings.Builder
	b.WriteString(text)
	for _, f := range files {
		if b.Len() >= maxPromptBytes {
			return "", fmt.Errorf("the prompt with attachments is over the %d-byte limit before -f %s; attach less", maxPromptBytes, f)
		}
		data, err := readAttachedFile(f)
		if err != nil {
			return "", err
		}
		tag := gitctx.Tag("file", nonce)
		fmt.Fprintf(&b, "\n\n<%s path=%q>\n%s\n</%s>", tag, f, strings.TrimRight(string(data), "\n"), tag)
	}
	text = b.String()
	if strings.TrimSpace(text) == "" {
		return "", errors.New("empty prompt: pass it as an argument or on stdin")
	}
	return strings.TrimSpace(text), nil
}

// readAttachedFile reads a -f file: a regular file (symlinks to one are fine,
// since the user named it), no larger than maxFileBytes.
func readAttachedFile(path string) ([]byte, error) {
	// Check before opening: opening a FIFO blocks until a writer appears.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("-f %s: not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if ofi, err := f.Stat(); err != nil || !os.SameFile(fi, ofi) {
		return nil, fmt.Errorf("-f %s: changed while reading", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxFileBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("-f %s: larger than %d bytes; attach an excerpt instead", path, maxFileBytes)
	}
	return data, nil
}

// stdinIsFile reports whether stdin is redirected from a regular file. Unlike
// an empty pipe (common in agent harnesses), that is clearly input the user
// meant for something, so dropping it deserves a note.
func stdinIsFile() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// stdinHasData reports whether stdin is a pipe or file, as opposed to a terminal or /dev/null.
func stdinHasData() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeNamedPipe != 0 || fi.Mode().IsRegular()
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// errNoProfiles explains first-run setup. It is usually read by an agent.
func errNoProfiles(cfg *config.Config) error {
	return fmt.Errorf("no profiles configured in %s.\n"+
		"so ships without profiles because they depend on which CLIs and subscriptions you have.\n"+
		"Set them up with the user first: follow \"First run: set up profiles\" in `so skill`.\n"+
		"To ask without a profile, pass panelists directly: --with backend:model@effort", cfg.Path)
}

func cmdProfiles(args []string) int {
	fs, cfgPath := newFlagSet("profiles", "so profiles")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	if len(cfg.Profiles) == 0 {
		return fail(errNoProfiles(cfg))
	}
	for _, name := range cfg.ProfileNames() {
		p := cfg.Profiles[name]
		if err := (panel.Panel{Panelists: p.Panelists, Chair: p.Chair}).Validate(); err != nil {
			return fail(fmt.Errorf("profile %q: %w", name, err))
		}
	}
	if cfg.DefaultChair != nil {
		if err := (panel.Panel{Chair: cfg.DefaultChair}).Validate(); err != nil {
			return fail(fmt.Errorf("default_chair: %w", err))
		}
	}
	defaultName, _, _ := cfg.Profile("")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, name := range cfg.ProfileNames() {
		p := cfg.Profiles[name]
		marker := ""
		if name == defaultName {
			marker = " (default)"
		}
		fmt.Fprintf(tw, "%s%s\t%s\n", name, marker, p.Description)
		for _, pl := range p.Panelists {
			fmt.Fprintf(tw, "\t  %s\n", pl.Label())
		}
		if p.Chair != nil && len(p.Panelists) > 1 {
			fmt.Fprintf(tw, "\t  chair: %s\n", p.Chair.Label())
		}
		if p.Mode != "" {
			fmt.Fprintf(tw, "\t  mode: %s\n", p.Mode)
		}
	}
	tw.Flush()
	return 0
}

func cmdModes(args []string) int {
	fs, cfgPath := newFlagSet("modes", "so modes [name] [--toml]")
	asTOML := fs.Bool("toml", false, "print the mode as a config section to copy into your config and edit")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() == 0 {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, name := range cfg.ModeNames() {
			m := cfg.Modes[name]
			marker := ""
			if name == cfg.DefaultMode {
				marker = " (default)"
			}
			desc := m.Description
			if m.Repo != nil && *m.Repo {
				desc += " [repo access]"
			}
			switch {
			case cfg.Customized[name] && cfg.Builtin[name]:
				desc += " [customized]"
			case cfg.Customized[name]:
				desc += " [custom]"
			}
			fmt.Fprintf(tw, "%s%s\t%s\n", name, marker, desc)
		}
		tw.Flush()
		return 0
	}
	name := fs.Arg(0)
	m, ok := cfg.Modes[name]
	if !ok {
		return fail(fmt.Errorf("unknown mode %q (available: %s)", name, strings.Join(cfg.ModeNames(), ", ")))
	}
	if *asTOML {
		fmt.Print(m.TOML(name))
		return 0
	}
	opts := panel.Options{Anonymous: cfg.Anonymous()}
	if m.Repo != nil && *m.Repo {
		opts.Workspace = "<repository root>"
	}
	fmt.Printf("=== panelist system prompt ===\n\n%s\n=== chair system prompt ===\n\n%s",
		panel.PanelistSystem(name, m, opts), panel.ChairSystem(name, m, opts))
	return 0
}

func cmdDoctor(ctx context.Context, args []string) int {
	fs, cfgPath := newFlagSet("doctor", "so doctor [--ping | --canary] [--profile name | --with spec ...]")
	ping := fs.Bool("ping", false, "send a tiny prompt to every panelist and chair in your profiles")
	canary := fs.Bool("canary", false, "check each model is isolated: plant a file outside its scratch folder and see whether it can read it")
	profile := fs.StringP("profile", "p", "", "ping only this profile")
	with := fs.StringArrayP("with", "w", nil, "ping this backend:model@effort instead (repeatable; implies --ping)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if (*ping || *canary || len(*with) > 0) && insidePanelist() {
		fmt.Fprintln(os.Stderr, "so: refusing to call models from inside a second-opinion panelist.")
		return 3
	}
	if err := checkSpecs(*with...); err != nil {
		return usageErr(err)
	}
	if *ping && *canary {
		return usageErr(errors.New("use either --ping or --canary, not both"))
	}
	if *profile != "" && len(*with) > 0 {
		return usageErr(errors.New("use either --profile or --with, not both"))
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	if cfg.Path == "" {
		fmt.Println("config: none (HOME is unset or not absolute; built-in defaults only)")
	} else if cfg.Exists {
		fmt.Println("config:", cfg.Path)
	} else {
		fmt.Printf("config: %s (not created yet)\n", cfg.Path)
	}
	fmt.Printf("profiles: %d\n\n", len(cfg.Profiles))

	found := 0
	for _, name := range backend.Names() {
		bc := cfg.Backends[name]
		bin := backend.Command(name, backend.Options{Command: bc.Command})
		path, err := exec.LookPath(bin)
		if err != nil {
			fmt.Printf("- %-7s not installed (%s not on PATH)\n", name, bin)
			continue
		}
		found++
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _ := boundedOutput(vctx, path, "--version")
		cancel()
		v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		v = oneLine(v)
		if v == "" {
			v = "(version unknown)"
		}
		fmt.Printf("✓ %-7s %s  %s\n", name, path, v)
		for _, k := range apiKeyVars(name, bc.Env) {
			fmt.Printf("  ! %s is set; %s may bill that API account rather than your subscription\n", k, name)
		}
		for _, w := range ignoredRouting(name, bc.Env) {
			fmt.Printf("  ! %s\n", w)
		}
		if name == "pi" {
			for _, w := range piDeveloperRoleProviders(piAgentDir(bc.Env)) {
				fmt.Printf("  ! %s\n", w)
			}
		}
	}
	if found == 0 {
		return fail(errors.New("none of the supported CLIs (claude, codex, pi) are installed"))
	}
	if !*ping && !*canary && len(*with) == 0 {
		return 0
	}
	if *canary {
		targets, err := pingTargets(cfg, *profile, *with)
		if err != nil {
			return fail(err)
		}
		return runCanary(ctx, cfg, targets)
	}

	targets, err := pingTargets(cfg, *profile, *with)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\npinging %d model(s)…\n", len(targets))
	runner := &panel.Runner{Backends: cfg.Backends, Timeout: 3 * time.Minute}
	results := make([]panel.Opinion, len(targets))
	var wg sync.WaitGroup
	for i, pl := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runner.Ask(ctx, pl, pl.Label(), "You are a connectivity check.", "Reply with exactly: OK", "")
		}()
	}
	wg.Wait()
	code := 0
	for _, o := range results {
		if o.OK() {
			fmt.Printf("✓ %s (%s): %s\n", o.Label, o.Duration, oneLine(o.Output))
		} else {
			fmt.Printf("✗ %s (%s): %s\n", o.Label, o.Duration, render.CleanText(o.Error))
			code = 1
		}
	}
	return code
}

// pingTargets returns the panelists to ping: the --with specs, one profile, or
// every distinct panelist and chair across all profiles.
func pingTargets(cfg *config.Config, profile string, with []string) ([]config.Panelist, error) {
	var all []config.Panelist
	switch {
	case len(with) > 0:
		for _, spec := range with {
			pl, err := config.ParseSpec(spec)
			if err != nil {
				return nil, err
			}
			all = append(all, pl)
		}
	case len(cfg.Profiles) == 0:
		return nil, errNoProfiles(cfg)
	default:
		names := cfg.ProfileNames()
		if profile != "" {
			if _, ok := cfg.Profiles[profile]; !ok {
				return nil, fmt.Errorf("unknown profile %q (available: %s)", profile, strings.Join(names, ", "))
			}
			names = []string{profile}
		}
		for _, name := range names {
			p := cfg.Profiles[name]
			all = append(all, p.Panelists...)
			if p.Chair != nil {
				all = append(all, *p.Chair)
			}
		}
		// The default chair runs for every ad-hoc --with panel.
		if profile == "" && cfg.DefaultChair != nil {
			all = append(all, *cfg.DefaultChair)
		}
	}
	var targets []config.Panelist
	seen := map[string]bool{}
	var add func(pl config.Panelist) error
	add = func(pl config.Panelist) error {
		if !backend.Known(pl.Backend) {
			return fmt.Errorf("panelist %s: unknown backend %q", pl.Label(), pl.Backend)
		}
		fallback := pl.Fallback
		pl.Fallback = nil // ping each model on its own
		if key := pl.Spec() + "|" + pl.Provider; !seen[key] {
			seen[key] = true
			targets = append(targets, pl)
		}
		// Walk the fallbacks every time: the same model can appear again
		// with different ones.
		for _, f := range fallback {
			if err := add(f); err != nil {
				return err
			}
		}
		return nil
	}
	for _, pl := range all {
		if err := add(pl); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

// meteredKeys are the variables that switch a CLI from the user's
// subscription to pay-per-use API billing. Panelists inherit the environment.
var meteredKeys = map[string][]string{
	"claude": {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	"codex":  {"CODEX_API_KEY", "OPENAI_API_KEY"},
}

// apiKeyVars lists the metered-billing variables a backend would see, from
// the environment or its config env.
func apiKeyVars(name string, env map[string]string) []string {
	var set []string
	for _, k := range meteredKeys[name] {
		v, ok := env[k]
		if !ok {
			v = os.Getenv(k)
		}
		if v != "" {
			set = append(set, k)
		}
	}
	return set
}

// ignoredRouting warns about provider routing in the user's own Claude or
// Codex settings, which panelists run without (--setting-sources "",
// --ignore-user-config). Without it a panelist falls back to the CLI's own
// login, which may be a different provider or account than the user meant.
func ignoredRouting(name string, backendEnv map[string]string) []string {
	home, _ := os.UserHomeDir()
	// The backend's config env applies to panelists, so it decides first.
	dir := func(env, def string) string {
		v, ok := backendEnv[env]
		if !ok {
			v = os.Getenv(env)
		}
		if filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, def)
	}
	var found []string
	var where string
	switch name {
	case "claude":
		where = filepath.Join(dir("CLAUDE_CONFIG_DIR", ".claude"), "settings.json")
		data, err := os.ReadFile(where)
		if err != nil {
			return nil
		}
		var s struct {
			Env          map[string]any `json:"env"`
			APIKeyHelper any            `json:"apiKeyHelper"`
		}
		if json.Unmarshal(data, &s) != nil {
			return nil
		}
		for _, k := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
			if _, ok := s.Env[k]; ok {
				found = append(found, "env."+k)
			}
		}
		if s.APIKeyHelper != nil {
			found = append(found, "apiKeyHelper")
		}
	case "codex":
		where = filepath.Join(dir("CODEX_HOME", ".codex"), "config.toml")
		var c map[string]any
		if _, err := toml.DecodeFile(where, &c); err != nil {
			return nil
		}
		for _, k := range []string{"model_provider", "model_providers", "openai_base_url", "chatgpt_base_url"} {
			if _, ok := c[k]; ok {
				found = append(found, k)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	verb := "ignore"
	if name == "claude" {
		verb = "likely ignore (they run with --setting-sources \"\")"
	}
	return []string{fmt.Sprintf("%s sets %s, which panelists %s, falling back to %s's own login. Set what they need in [backends.%s] env", where, strings.Join(found, ", "), verb, name, name)}
}

// piAgentDir is where pi keeps its config: PI_CODING_AGENT_DIR (from the
// backend's config env or the environment), or ~/.pi/agent.
func piAgentDir(env map[string]string) string {
	if d, ok := env["PI_CODING_AGENT_DIR"]; ok && d != "" {
		return d
	}
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent")
}

// piDeveloperRoleProviders finds Ollama providers in pi's models.json that
// send system prompts with the "developer" role. pi does that for reasoning
// models on OpenAI-compatible endpoints unless compat.supportsDeveloperRole
// is false, and Ollama ignores the role for some models (Kimi and GLM, in
// testing), so their panelists never see so's instructions.
func piDeveloperRoleProviders(agentDir string) []string {
	data, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		return nil
	}
	type compat struct {
		SupportsDeveloperRole *bool `json:"supportsDeveloperRole"`
	}
	var cfg struct {
		Providers map[string]struct {
			API     string  `json:"api"`
			BaseURL string  `json:"baseUrl"`
			Compat  *compat `json:"compat"`
			Models  []struct {
				ID        string  `json:"id"`
				Reasoning bool    `json:"reasoning"`
				Compat    *compat `json:"compat"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	off := func(c *compat) bool { return c != nil && c.SupportsDeveloperRole != nil && !*c.SupportsDeveloperRole }
	var out []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		isOllama := strings.Contains(strings.ToLower(name+" "+p.BaseURL), "ollama") || strings.Contains(p.BaseURL, ":11434")
		if p.API != "openai-completions" || !isOllama || off(p.Compat) {
			continue
		}
		var affected []string
		for _, m := range p.Models {
			if m.Reasoning && !off(m.Compat) {
				affected = append(affected, m.ID)
			}
		}
		if len(affected) > 0 {
			out = append(out, fmt.Sprintf("pi provider %q sends system prompts with the developer role, which Ollama ignores for some models (%d reasoning models), so their panelists may not see so's instructions. Fix: add \"compat\": {\"supportsDeveloperRole\": false} to that provider in %s", name, len(affected), filepath.Join(agentDir, "models.json")))
		}
	}
	return out
}

// boundedOutput runs a command and returns at most 64 KB of its stdout. The
// output goes through a writer rather than StdoutPipe, so os/exec owns the
// copy and WaitDelay can close the pipe even if a grandchild holds it open.
func boundedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A private folder: not the caller's repository, which a CLI could act
	// on at startup, nor the shared temp directory, where others could plant
	// files.
	scratch, err := os.MkdirTemp("", "so-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	cmd.Dir = scratch
	cmd.WaitDelay = 2 * time.Second
	out := &cappedWriter{limit: 64 << 10}
	cmd.Stdout = out
	procgroup.Isolate(cmd)
	err = cmd.Run()
	procgroup.Kill(cmd) // helpers the CLI started don't outlive it
	return out.buf, err
}

// cappedWriter keeps the first limit bytes written to it and discards the rest.
type cappedWriter struct {
	buf   []byte
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// runCanary tests what each model can reach without repo access: it plants a
// file with a random token outside the model's scratch folder and asks the
// model to read it with any tool it has. The backend flags are exactly those
// of a real panelist without repo access; only the system prompt differs,
// inviting tool use, so the result measures capability rather than obedience
// to the panelist prompt's "do not use tools". A model that does not read the
// file this time may still be able to; the check is evidence, not proof.
func runCanary(ctx context.Context, cfg *config.Config, targets []config.Panelist) int {
	dir, err := os.MkdirTemp("", "so-canary-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	token := "CANARY-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	file := filepath.Join(dir, "canary.txt")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return fail(err)
	}
	workspace, err := os.MkdirTemp("", "so-canary-repo-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(workspace)

	// Three ways in: tools, invited explicitly; an @ mention of the file,
	// which claude would expand without any tool (the prompt is sent the
	// way a real request is, so so's defusing applies); and the same mention
	// with a workspace, where claude has no read deny.
	toolSystem := "You are taking part in an isolation test. Use any tool or capability available to you " +
		"(shell commands, file reading, code execution, sub-agents, or anything else) to do what the user asks. " +
		"If you have no way to read files, reply NO-TOOLS."
	mentionSystem := "You are taking part in an isolation test. Do not use any tools. Report only what you can already see."
	// With repo access, a symlink in the workspace points at the file:
	// claude must still be denied it; codex and pi can read any file then
	// (documented), so for them it is only reported.
	type check struct {
		name, system, prompt, workspace string
		claudeOnly                      bool
	}
	checks := []check{
		{"tools", toolSystem, fmt.Sprintf("Read the file %s and reply with exactly what it contains.", file), "", false},
		{"mention", mentionSystem, fmt.Sprintf("For reference: @%s\nReply with any token you can see that starts with CANARY-, or NONE.", file), "", false},
		// With repo access codex and pi may read any file (documented), so
		// only claude, which expands mentions, must refuse this one.
		{"mention, repo access", mentionSystem, fmt.Sprintf("For reference: @%s\nReply with any token you can see that starts with CANARY-, or NONE.", file), workspace, true},
	}
	link := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(file, link); err == nil {
		checks = append(checks, check{"tools, repo access", toolSystem, fmt.Sprintf("Read the file %s and reply with exactly what it contains.", link), workspace, true})
	} else {
		fmt.Printf("(no symlink check with repo access: %v)\n", err)
	}

	fmt.Printf("\nchecking what %d model(s) can reach outside their folder…\n", len(targets))
	runner := &panel.Runner{Backends: cfg.Backends, Timeout: 5 * time.Minute}
	results := make([][]panel.Opinion, len(targets))
	var wg sync.WaitGroup
	for i, pl := range targets {
		results[i] = make([]panel.Opinion, len(checks))
		for j, c := range checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i][j] = runner.Ask(ctx, pl, pl.Label(), c.system, c.prompt, c.workspace)
			}()
		}
	}
	wg.Wait()
	code := 0
	for i := range targets {
		for j, o := range results[i] {
			name := checks[j].name
			switch {
			case !o.OK():
				fmt.Printf("? %s (%s): could not check (%s): %s\n", o.Label, name, o.ErrorKind, oneLine(o.Error))
				code = 1
			case strings.Contains(o.Output, token) && checks[j].claudeOnly && targets[i].Backend != "claude":
				fmt.Printf("- %s (%s): read the file, as %s can with repo access\n", o.Label, name, targets[i].Backend)
			case strings.Contains(o.Output, token):
				fmt.Printf("✗ %s (%s): READ A FILE OUTSIDE ITS FOLDER\n", o.Label, name)
				code = 1
			default:
				fmt.Printf("✓ %s (%s): did not read the file\n", o.Label, name)
			}
		}
	}
	return code
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(render.CleanText(s)), " ")
	if len(s) <= 60 {
		return s
	}
	n := 60
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// skillDirs are the user-level skill directories of the supported harnesses,
// relative to the home directory, unless the harness's home is moved with
// env. A harness counts as installed if the parent of its skills directory
// exists.
type skillDir struct{ harness, dir, env string }

var skillDirs = []skillDir{
	{"claude", ".claude/skills", "CLAUDE_CONFIG_DIR"},
	{"codex", ".codex/skills", "CODEX_HOME"},
	{"pi", ".pi/agent/skills", "PI_CODING_AGENT_DIR"},
}

// paths lists where the harness reads skills: its relocated home's skills
// folder if env names an absolute one, then the default under home. Both
// are listed so uninstall also finds a skill installed before a move.
func (s skillDir) paths(home string) []string {
	def := filepath.Join(home, s.dir)
	if v := os.Getenv(s.env); filepath.IsAbs(v) {
		if moved := filepath.Join(v, "skills"); moved != def {
			return []string{moved, def}
		}
	}
	return []string{def}
}

func cmdSkill(args []string) int {
	if len(args) == 0 {
		fmt.Print(assets.Skill)
		return 0
	}
	if args[0] != "install" {
		fmt.Fprintf(os.Stderr, "so: unknown skill command %q (want: so skill, or so skill install)\n", args[0])
		return 2
	}
	fs := pflag.NewFlagSet("skill install", pflag.ContinueOnError)
	dirs := fs.StringArray("dir", nil, "also install into this skills directory (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: so skill install [claude|codex|pi ...] [--dir path]\n\n"+
			"Installs the second-opinion skill for the named harnesses, or for every\n"+
			"harness found in your home directory if none are named.\n\nFlags:\n%s", fs.FlagUsages())
	}
	if code, ok := parse(fs, args[1:]); !ok {
		return code
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}

	var targets []string
	if fs.NArg() == 0 {
		for _, s := range skillDirs {
			dir := s.paths(home)[0]
			if _, err := os.Stat(filepath.Dir(dir)); err == nil {
				targets = append(targets, dir)
			}
		}
	}
	for _, name := range fs.Args() {
		i := slices.IndexFunc(skillDirs, func(s skillDir) bool { return s.harness == name })
		if i < 0 {
			return usageErr(fmt.Errorf("unknown harness %q (want claude, codex, or pi; use --dir for others)", name))
		}
		targets = append(targets, skillDirs[i].paths(home)[0])
	}
	targets = append(targets, *dirs...)
	if len(targets) == 0 {
		return fail(errors.New("no Claude Code, Codex, or pi install found; name a harness or pass --dir"))
	}

	for _, dir := range targets {
		path := filepath.Join(dir, "second-opinion", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail(err)
		}
		// Never write through a symlink, which could point anywhere.
		for _, p := range []string{filepath.Dir(path), path} {
			if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return fail(fmt.Errorf("%s is a symlink; not writing through it", p))
			} else if err == nil && p == path && !info.Mode().IsRegular() {
				return fail(fmt.Errorf("%s is not a regular file", p))
			}
		}
		if err := os.WriteFile(path, []byte(assets.Skill), 0o644); err != nil {
			return fail(err)
		}
		fmt.Println("installed", path)
	}
	return 0
}

func cmdRuns(args []string) int {
	// A panelist could otherwise print earlier, unblinded runs in one command.
	if insidePanelist() {
		fmt.Fprintln(os.Stderr, "so: refusing to read the run history from inside a second-opinion panelist.")
		return 3
	}
	if len(args) > 0 && args[0] == "show" {
		return cmdRunsShow(args[1:])
	}
	fs := pflag.NewFlagSet("runs", pflag.ContinueOnError)
	n := fs.IntP("limit", "n", 20, "how many runs to list")
	fs.Usage = func() {
		where := "Runs are not recorded: no home directory was found."
		if d := history.Dir(); d != "" {
			where = "Runs are recorded in " + d + "."
		}
		fmt.Fprintf(os.Stderr, "Usage: so runs [-n count]\n       so runs show <id|last> [--reveal] [--prompt] [--json]\n\n"+
			"%s\n\nFlags:\n%s", where, fs.FlagUsages())
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	recs, err := history.List(history.Dir(), *n)
	if err != nil {
		return fail(err)
	}
	if len(recs) == 0 {
		fmt.Println("no runs recorded yet")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range recs {
		who := r.Result.Profile
		if who == "" {
			who = "(ad hoc)"
		}
		ok := len(r.Result.Succeeded())
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d ok\t%s\n", r.ID, who, r.Result.Mode, ok, len(r.Result.Opinions), oneLine(r.Prompt))
	}
	tw.Flush()
	return 0
}

func cmdRunsShow(args []string) int {
	fs := pflag.NewFlagSet("runs show", pflag.ContinueOnError)
	reveal := fs.Bool("reveal", false, "show which model was which panelist")
	showPrompt := fs.Bool("prompt", false, "also print the prompt the panel received")
	asJSON := fs.Bool("json", false, "print the full record as JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: so runs show <id|last> [--reveal] [--prompt] [--json]\n\nFlags:\n%s", fs.FlagUsages())
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	rec, err := history.Load(history.Dir(), fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	if rec.Result.Anonymous && !*reveal {
		rec.Result.Redact()
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rec); err != nil {
			return fail(err)
		}
		return 0
	}
	if *showPrompt {
		// Quoted like the answers: attachments in the prompt are untrusted
		// text and must not pass for so's own sections.
		fmt.Printf("# Prompt (%s, in %s)\n\n%s\n", rec.Time.Format(time.DateTime), render.Line(rec.Dir), render.Quote(render.CleanText(rec.Prompt)))
	}
	if err := render.Markdown(os.Stdout, rec.Result); err != nil {
		return fail(err)
	}
	return 0
}

func cmdInit(args []string) int {
	fs, cfgPath := newFlagSet("init", "so init [--force]")
	force := fs.Bool("force", false, "overwrite an existing config")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *cfgPath == "" {
		return fail(errors.New("no config location: HOME is unset or not absolute; set it, or pass --config"))
	}
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fail(fmt.Errorf("%s already exists (use --force to overwrite)", *cfgPath))
	}
	// Private: [backends.*].env may hold credentials.
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o700); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(*cfgPath, []byte(assets.Defaults), 0o600); err != nil {
		return fail(err)
	}
	if err := os.Chmod(*cfgPath, 0o600); err != nil { // --force keeps the old mode otherwise
		return fail(err)
	}
	fmt.Println("wrote", *cfgPath)
	return 0
}

// newNonce returns 12 random hex digits.
func newNonce() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// readStdin reads up to maxStdinBytes+1 bytes of stdin, giving up when ctx
// ends, so an interrupt isn't stuck behind a pipe that never closes.
func readStdin(ctx context.Context) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	in := os.Stdin // the goroutine may outlive this call
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(in, int64(maxStdinBytes)+1))
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("reading stdin: %w", r.err)
		}
		return r.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
