// Package panel fans a prompt out to a set of panelists and has a chair synthesize their answers.
package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/config"
)

// Panel is a resolved set of panelists, an optional chair, and the mode shaping their responses.
type Panel struct {
	Profile   string // "" for an ad-hoc panel
	ModeName  string
	Mode      config.Mode
	Panelists []config.Panelist
	Chair     *config.Panelist
	// Workspace is a repository the panel may read, or "" for no file access.
	Workspace string
	// Anonymous hides which model is which: panelists are labelled with letters
	// assigned at random for each run, in the chair's prompt and in the result.
	Anonymous bool
}

func (p Panel) Validate() error {
	all := p.Panelists
	if p.Chair != nil {
		all = append(all[:len(all):len(all)], *p.Chair)
	}
	for _, pl := range all {
		for _, c := range append([]config.Panelist{pl}, pl.Fallback...) {
			if !backend.Known(c.Backend) {
				return fmt.Errorf("panelist %s: unknown backend %q (available: %s)",
					c.Label(), c.Backend, strings.Join(backend.Names(), ", "))
			}
		}
	}
	return nil
}

type Opinion struct {
	Label string `json:"label"`
	// Panelist identifies the model that answered (or last failed). Redact
	// clears it for anonymous results.
	Panelist  *config.Panelist `json:"panelist,omitempty"`
	Output    string           `json:"output,omitempty"`
	Error     string           `json:"error,omitempty"`
	ErrorKind ErrorKind        `json:"error_kind,omitempty"`
	// Attempts are the models that hit a usage limit before a fallback took over.
	Attempts []Attempt     `json:"attempts,omitempty"`
	Duration time.Duration `json:"-"`
	Seconds  float64       `json:"seconds"`
}

func (o Opinion) OK() bool { return o.Error == "" }

// Attempt is a model call that failed before a fallback was tried.
type Attempt struct {
	Panelist *config.Panelist `json:"panelist,omitempty"`
	Kind     ErrorKind        `json:"kind"`
	Error    string           `json:"error,omitempty"`
}

type Result struct {
	// Run is the ID under which the run is recorded in the local history.
	Run       string    `json:"run,omitempty"`
	Profile   string    `json:"profile,omitempty"`
	Mode      string    `json:"mode"`
	Anonymous bool      `json:"anonymous,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Attached  string    `json:"attached,omitempty"` // summary of context gathered for the caller, e.g. a git diff
	Opinions  []Opinion `json:"opinions"`
	Synthesis *Opinion  `json:"synthesis,omitempty"`
	// ChairSkipped explains why a configured chair did not run.
	ChairSkipped string `json:"chair_skipped,omitempty"`
}

// Redact removes model identities, leaving only the anonymous labels.
// Attempt errors are dropped too, because they name the model that failed.
func (r *Result) Redact() {
	for i := range r.Opinions {
		r.Opinions[i].redact(r.Run)
	}
	if r.Synthesis != nil {
		r.Synthesis.redact(r.Run)
	}
}

func (o *Opinion) redact(run string) {
	o.Panelist = nil
	for i := range o.Attempts {
		o.Attempts[i] = Attempt{Kind: o.Attempts[i].Kind}
	}
	// A CLI's error names the backend and often the model; keep only its kind,
	// and say where the full error can be seen.
	switch {
	case o.Error == "":
	case run != "":
		o.Error = fmt.Sprintf("failed (%s); see the full error with: so runs show %s --reveal", o.ErrorKind, run)
	default:
		o.Error = fmt.Sprintf("failed (%s); the run was not recorded, so rerun with --reveal to see the full error", o.ErrorKind)
	}
}

func (r Result) Succeeded() []Opinion {
	var ok []Opinion
	for _, o := range r.Opinions {
		if o.OK() {
			ok = append(ok, o)
		}
	}
	return ok
}

type Runner struct {
	Backends map[string]config.Backend
	Timeout  time.Duration
	Progress io.Writer // nil disables progress output

	mu sync.Mutex
}

// Run asks every panelist in parallel, then has the chair (if any) synthesize the answers.
func (r *Runner) Run(ctx context.Context, p Panel, prompt string) Result {
	res := Result{
		Profile:   p.Profile,
		Mode:      p.ModeName,
		Anonymous: p.Anonymous,
		Workspace: p.Workspace,
		Opinions:  make([]Opinion, len(p.Panelists)),
	}
	opts := Options{Workspace: p.Workspace, Anonymous: p.Anonymous}
	system := PanelistSystem(p.ModeName, p.Mode, opts)

	// slot[i] is where panelist i's opinion goes. Anonymous panels shuffle the
	// slots and label them by position, so neither letter nor order reveals
	// which model answered.
	labels := uniqueLabels(p.Panelists)
	slot := make([]int, len(p.Panelists))
	for i := range slot {
		slot[i] = i
	}
	if p.Anonymous {
		slot = rand.Perm(len(p.Panelists))
		for i, s := range slot {
			labels[i] = "Panelist " + letter(s)
		}
	}

	// Start lines go out in letter order, before any panelist runs, so their
	// order can't hint at the profile's order either.
	order := make([]int, len(slot)) // order[s] is the panelist in slot s
	for i, s := range slot {
		order[s] = i
	}
	for _, i := range order {
		r.progress("… %s", labels[i])
	}
	var wg sync.WaitGroup
	for _, i := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.Opinions[slot[i]] = r.ask(ctx, p.Panelists[i], labels[i], system, prompt, p.Workspace)
		}()
	}
	wg.Wait()

	if p.Chair == nil {
		return res
	}
	if ctx.Err() != nil {
		res.ChairSkipped = "cancelled"
		return res
	}
	ok := res.Succeeded()
	if len(ok) < 2 {
		res.ChairSkipped = fmt.Sprintf("only %d of %d panelists responded", len(ok), len(p.Panelists))
		return res
	}
	label := "chair " + p.Chair.Label()
	if p.Anonymous {
		label = "Chair"
	}
	syn := r.Ask(ctx, *p.Chair, label, ChairSystem(p.ModeName, p.Mode, opts), ChairPrompt(prompt, ok), p.Workspace)
	res.Synthesis = &syn
	return res
}

// Ask sends one prompt to one panelist, with read-only access to workspace if
// it is not "". If the panelist hits a usage limit, its fallbacks are tried in order.
func (r *Runner) Ask(ctx context.Context, pl config.Panelist, label, system, prompt, workspace string) Opinion {
	r.progress("… %s", label)
	return r.ask(ctx, pl, label, system, prompt, workspace)
}

// ask is Ask without the start line.
func (r *Runner) ask(ctx context.Context, pl config.Panelist, label, system, prompt, workspace string) Opinion {
	op := Opinion{Label: label}
	start := time.Now()
	candidates := append([]config.Panelist{pl}, pl.Fallback...)
	for i, c := range candidates {
		c.Fallback = nil
		op.Panelist = &c
		out, err := r.call(ctx, c, system, prompt, workspace)
		if err == nil {
			op.Output = out
			break
		}
		kind := classify(err)
		if kind == KindQuota && i < len(candidates)-1 && ctx.Err() == nil {
			op.Attempts = append(op.Attempts, Attempt{Panelist: &c, Kind: kind, Error: err.Error()})
			r.progress("↻ %s hit a usage limit; trying its fallback", label)
			continue
		}
		op.Error, op.ErrorKind = err.Error(), kind
		break
	}
	op.Duration = time.Since(start).Round(100 * time.Millisecond)
	op.Seconds = op.Duration.Seconds()
	if !op.OK() {
		r.progress("✗ %s failed after %s (%s)", label, op.Duration, op.ErrorKind)
		return op
	}
	r.progress("✓ %s (%s)", label, op.Duration)
	return op
}

func (r *Runner) call(ctx context.Context, pl config.Panelist, system, prompt, workspace string) (string, error) {
	bc := r.Backends[pl.Backend]
	b, err := backend.New(pl.Backend, backend.Options{Command: bc.Command, Args: bc.Args, Env: bc.Env})
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "second-opinion-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	out, err := b.Run(ctx, backend.Request{
		System:    system,
		Prompt:    prompt,
		Model:     pl.Model,
		Effort:    pl.Effort,
		Provider:  pl.Provider,
		Dir:       dir,
		Workspace: workspace,
	})
	if err == nil && strings.TrimSpace(out) == "" {
		err = errors.New("empty response")
	}
	return out, err
}

func (r *Runner) progress(format string, args ...any) {
	if r.Progress == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.Progress, "so: "+format+"\n", args...)
}

// letter returns A, B, …, Z, AA, AB, … for 0, 1, ….
func letter(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

// uniqueLabels suffixes duplicate labels with #2, #3, … so responses stay distinguishable.
func uniqueLabels(ps []config.Panelist) []string {
	labels := make([]string, len(ps))
	seen := map[string]int{}
	for i, p := range ps {
		l := p.Label()
		seen[l]++
		if n := seen[l]; n > 1 {
			l = fmt.Sprintf("%s#%d", l, n)
		}
		labels[i] = l
	}
	return labels
}
