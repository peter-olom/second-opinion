// Command so gets second opinions from other AI models through the agent CLIs you already use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/spf13/pflag"

	"github.com/peter-olom/second-opinion/internal/assets"
	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/config"
	"github.com/peter-olom/second-opinion/internal/gitctx"
	"github.com/peter-olom/second-opinion/internal/history"
	"github.com/peter-olom/second-opinion/internal/panel"
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
	mode := fs.StringP("mode", "m", "", "response mode, e.g. general, review, plan, debug")
	files := fs.StringArrayP("file", "f", nil, "attach a file to the prompt (repeatable)")
	diff := fs.Bool("diff", false, "attach the git diff (against --base) and the full contents of changed files")
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
		return fail(errors.New("--timeout must be positive"))
	}
	if *format != "md" && *format != "json" {
		return fail(fmt.Errorf("unknown format %q (want md or json)", *format))
	}
	if *profile != "" && len(*with) > 0 {
		return fail(errors.New("use either --profile or --with, not both"))
	}
	if *repo && *noRepo {
		return fail(errors.New("use either --repo or --no-repo, not both"))
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	p, err := resolvePanel(cfg, *profile, *mode, *with, *chair, *noChair)
	if err != nil {
		return fail(err)
	}
	prompt, err := readPrompt(fs.Args(), *files)
	if err != nil {
		return fail(err)
	}
	var attached string
	if *diff || *base != "" {
		// Same rule as repo access: never collect from a repository rooted
		// at home or above (a dotfiles repo in ~), whose untracked files
		// could be anything in $HOME.
		if root, err := gitctx.Root(ctx, "."); err == nil && containsHome(root) {
			return fail(errors.New("--diff would collect from a git repository rooted at your home directory; run so inside a project repository"))
		}
		gc, err := gitctx.Diff(ctx, ".", firstNonEmpty(*base, "HEAD"))
		if err != nil {
			return fail(err)
		}
		prompt += "\n\n" + gc.Text
		attached = gc.Summary
	}
	if len(prompt) > maxPromptBytes {
		return fail(fmt.Errorf("the prompt with attachments is %d bytes, over the %d-byte limit; attach less", len(prompt), maxPromptBytes))
	}
	if p.Workspace, err = resolveWorkspace(*repo, *noRepo, p.Mode.Repo); err != nil {
		return fail(err)
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
	res := runner.Run(ctx, p, prompt)
	res.Attached = attached
	if cfg.Recording() {
		res.Run = history.NewID(start)
		if err := saveRun(cfg, p, prompt, res, start); err != nil {
			fmt.Fprintln(os.Stderr, "so: could not record run:", err)
			res.Run = ""
		}
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
func resolveWorkspace(explicit, disabled bool, modeDefault *bool) (string, error) {
	if disabled || (!explicit && (modeDefault == nil || !*modeDefault)) {
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
	if root, err := gitctx.Root(context.Background(), cwd); err == nil {
		if !containsHome(root) {
			return root, nil
		}
		// A repository rooted at home or above (a dotfiles repo in ~) would
		// open up all of $HOME. A mode default never does that; an explicit
		// --repo gets the current folder instead, unless that is home itself.
		if !explicit {
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
	return "", nil
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
func readPrompt(args, files []string) (string, error) {
	// Decided from the arguments themselves, not their text, so an empty
	// argument (so ask "$q" with $q unset) never reads stdin.
	useStdin := len(args) == 0 || args[len(args)-1] == "-"
	if useStdin && len(args) > 0 {
		args = args[:len(args)-1]
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if !useStdin && stdinIsFile() {
		fmt.Fprintln(os.Stderr, "so: stdin was not read; end the arguments with - to attach it")
	}
	if useStdin && stdinHasData() {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(maxStdinBytes)+1))
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		if len(data) > maxStdinBytes {
			return "", fmt.Errorf("stdin is larger than %d bytes; send a smaller excerpt", maxStdinBytes)
		}
		if in := strings.TrimSpace(string(data)); in != "" {
			if text == "" {
				text = in
			} else {
				text += "\n\n<stdin>\n" + in + "\n</stdin>"
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
		fmt.Fprintf(&b, "\n\n<file path=%q>\n%s\n</file>", f, strings.TrimRight(string(data), "\n"))
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
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	if cfg.Exists {
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
		fmt.Printf("✓ %-7s %s  %s\n", name, path, v)
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
			fmt.Printf("✗ %s (%s): %s\n", o.Label, o.Duration, o.Error)
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

// boundedOutput runs a command and returns at most 64 KB of its stdout.
func boundedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, _ := io.ReadAll(io.LimitReader(stdout, 64<<10))
	cmd.Process.Kill()
	cmd.Wait()
	return out, nil
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
	prompt := fmt.Sprintf("Read the file %s and reply with exactly what it contains.", file)
	system := "You are taking part in an isolation test. Use any tool or capability available to you " +
		"(shell commands, file reading, code execution, sub-agents, or anything else) to do what the user asks. " +
		"If you have no way to read files, reply NO-TOOLS."

	fmt.Printf("\nchecking what %d model(s) can reach without repo access…\n", len(targets))
	runner := &panel.Runner{Backends: cfg.Backends, Timeout: 5 * time.Minute}
	results := make([]panel.Opinion, len(targets))
	var wg sync.WaitGroup
	for i, pl := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runner.Ask(ctx, pl, pl.Label(), system, prompt, "")
		}()
	}
	wg.Wait()
	code := 0
	for _, o := range results {
		switch {
		case !o.OK():
			fmt.Printf("? %s: could not check (%s)\n", o.Label, o.ErrorKind)
			code = 1
		case strings.Contains(o.Output, token):
			fmt.Printf("✗ %s: READ A FILE OUTSIDE ITS SCRATCH FOLDER\n", o.Label)
			code = 1
		default:
			fmt.Printf("✓ %s: did not read the file\n", o.Label)
		}
	}
	return code
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// skillDirs are the user-level skill directories of the supported harnesses,
// relative to the home directory. A harness counts as installed if the parent
// of its skills directory exists.
var skillDirs = []struct{ harness, dir string }{
	{"claude", ".claude/skills"},
	{"codex", ".codex/skills"},
	{"pi", ".pi/agent/skills"},
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
			if _, err := os.Stat(filepath.Dir(filepath.Join(home, s.dir))); err == nil {
				targets = append(targets, filepath.Join(home, s.dir))
			}
		}
	}
	for _, name := range fs.Args() {
		i := slices.IndexFunc(skillDirs, func(s struct{ harness, dir string }) bool { return s.harness == name })
		if i < 0 {
			return fail(fmt.Errorf("unknown harness %q (want claude, codex, or pi; use --dir for others)", name))
		}
		targets = append(targets, filepath.Join(home, skillDirs[i].dir))
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
		if err := os.WriteFile(path, []byte(assets.Skill), 0o644); err != nil {
			return fail(err)
		}
		fmt.Println("installed", path)
	}
	return 0
}

func cmdRuns(args []string) int {
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
		fmt.Printf("# Prompt (%s, in %s)\n\n%s\n\n", rec.Time.Format(time.DateTime), rec.Dir, rec.Prompt)
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
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fail(fmt.Errorf("%s already exists (use --force to overwrite)", *cfgPath))
	}
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(*cfgPath, []byte(assets.Defaults), 0o644); err != nil {
		return fail(err)
	}
	fmt.Println("wrote", *cfgPath)
	return 0
}
