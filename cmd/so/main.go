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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
		return cmdUninstall(rest)
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
		"The prompt is taken from the arguments, or from stdin if no arguments are given.\n"+
		"If both are present, stdin is attached to the prompt (e.g. git diff | so ask -m review \"...\").")
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

	if os.Getenv(backend.EnvGuard) != "" {
		fmt.Fprintln(os.Stderr, "so: refusing to run inside a second-opinion panelist; answer the request yourself.")
		return 3
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
		gc, err := gitctx.Diff(".", firstNonEmpty(*base, "HEAD"))
		if err != nil {
			return fail(err)
		}
		prompt += "\n\n" + gc.Text
		attached = gc.Summary
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
// outside one. A mode's default only applies inside a git repository, so a panel
// never silently gets access to an arbitrary directory such as $HOME.
func resolveWorkspace(explicit, disabled bool, modeDefault *bool) (string, error) {
	if disabled || (!explicit && (modeDefault == nil || !*modeDefault)) {
		return "", nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, err := gitctx.Root(cwd); err == nil {
		return root, nil
	}
	if explicit {
		return cwd, nil
	}
	return "", nil
}

// readPrompt builds the prompt from arguments, stdin, and attached files.
func readPrompt(args, files []string) (string, error) {
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "-" {
		text = ""
	}
	if stdinHasData() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		if in := strings.TrimSpace(string(data)); in != "" {
			if text == "" {
				text = in
			} else {
				text += "\n\n<stdin>\n" + in + "\n</stdin>"
			}
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		text += fmt.Sprintf("\n\n<file path=%q>\n%s\n</file>", f, strings.TrimRight(string(data), "\n"))
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("empty prompt: pass it as an argument or on stdin")
	}
	return strings.TrimSpace(text), nil
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
	fs, cfgPath := newFlagSet("doctor", "so doctor [--ping] [--profile name | --with spec ...]")
	ping := fs.Bool("ping", false, "send a tiny prompt to every panelist and chair in your profiles")
	profile := fs.StringP("profile", "p", "", "ping only this profile")
	with := fs.StringArrayP("with", "w", nil, "ping this backend:model@effort instead (repeatable; implies --ping)")
	if code, ok := parse(fs, args); !ok {
		return code
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
		out, _ := exec.CommandContext(vctx, path, "--version").Output()
		cancel()
		v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		fmt.Printf("✓ %-7s %s  %s\n", name, path, v)
	}
	if found == 0 {
		return fail(errors.New("none of the supported CLIs (claude, codex, pi) are installed"))
	}
	if !*ping && len(*with) == 0 {
		return 0
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
		if key := pl.Spec() + "|" + pl.Provider; !seen[key] {
			seen[key] = true
			fallback := pl.Fallback
			pl.Fallback = nil // ping each model on its own
			targets = append(targets, pl)
			for _, f := range fallback {
				if err := add(f); err != nil {
					return err
				}
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
		fmt.Fprintf(os.Stderr, "Usage: so runs [-n count]\n       so runs show <id|last> [--reveal] [--prompt] [--json]\n\n"+
			"Runs are recorded in %s.\n\nFlags:\n%s", history.Dir(), fs.FlagUsages())
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

// executable locates the running binary. Tests replace it.
var executable = os.Executable

func cmdUninstall(args []string) int {
	fs, cfgPath := newFlagSet("uninstall", "so uninstall [--all] [--yes]\n\n"+
		"Removes the skills installed by `so skill install`, the run history, and the so binary.\n"+
		"Your config (profiles and mode overrides) is kept unless you pass --all.")
	all := fs.Bool("all", false, "also remove your config")
	yes := fs.BoolP("yes", "y", false, "do not ask for confirmation")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if os.Getenv(backend.EnvGuard) != "" {
		fmt.Fprintln(os.Stderr, "so: refusing to uninstall from inside a second-opinion panelist.")
		return 3
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}

	type item struct{ what, path string }
	var items []item
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}
	for _, s := range skillDirs {
		if p := filepath.Join(home, s.dir, "second-opinion"); exists(p) {
			items = append(items, item{s.harness + " skill", p})
		}
	}
	if state := filepath.Dir(history.Dir()); exists(state) {
		items = append(items, item{"run history", state})
	}
	if *all && exists(*cfgPath) {
		items = append(items, item{"config", *cfgPath})
	}
	// The binary goes last, and only if it really is so (not, say, a test binary).
	if exe, err := executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if name := filepath.Base(exe); name == "so" || name == "so.exe" {
			items = append(items, item{"binary", exe})
		}
	}

	if len(items) == 0 {
		fmt.Println("nothing to remove")
		return 0
	}
	fmt.Println("This removes:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, it := range items {
		fmt.Fprintf(tw, "  %s\t%s\n", it.what, it.path)
	}
	tw.Flush()
	if !*all && exists(*cfgPath) {
		fmt.Printf("Your config stays at %s (use --all to remove it too).\n", *cfgPath)
	}
	if !*yes {
		fmt.Print("Continue? [y/N] ")
		var answer string
		if _, err := fmt.Scanln(&answer); errors.Is(err, io.EOF) {
			fmt.Println()
			return fail(errors.New("no confirmation received; re-run with --yes to remove these"))
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Println("cancelled; nothing was removed")
			return 1
		}
	}

	code := 0
	for _, it := range items {
		var err error
		switch it.what {
		case "config", "binary":
			err = os.Remove(it.path)
		default:
			err = os.RemoveAll(it.path)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "so: could not remove %s: %v\n", it.path, err)
			code = 1
			continue
		}
		fmt.Println("removed", it.path)
		// Tidy the config directory if removing the config emptied it.
		if dir := filepath.Dir(it.path); it.what == "config" && filepath.Base(dir) == "second-opinion" {
			os.Remove(dir)
		}
	}
	fmt.Println("Skills installed elsewhere with `so skill install --dir` are not tracked; remove those yourself.")
	return code
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
