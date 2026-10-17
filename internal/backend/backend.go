// Package backend runs a single prompt through an agent CLI in headless mode.
//
// Every backend is invoked without sessions or MCP servers, and with as little
// of the user's own configuration as each CLI allows (pi still uses your pi
// settings, and Codex your global AGENTS.md). By default it also has no tools and runs in an empty scratch directory, so the
// model answers only from the prompt. With a Workspace it runs there instead,
// with read-only tools to search and read the code.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/peter-olom/second-opinion/internal/procgroup"
)

// ErrTimeout is returned when a CLI exceeds its deadline.
var ErrTimeout = errors.New("timed out")

// EnvGuard is set in every panelist's environment so that `so` refuses to run
// from inside a panelist and panels cannot recurse.
const EnvGuard = "SECOND_OPINION_DEPTH"

type Request struct {
	System   string
	Prompt   string
	Model    string
	Effort   string
	Provider string
	// Dir is an empty scratch directory. It is the working directory when there
	// is no Workspace, so the CLI does not pick up project files such as AGENTS.md.
	Dir string
	// Workspace, if set, is a repository the model may read (but not modify).
	Workspace string
	// Deny lists absolute paths in the workspace the model must not read.
	// claude enforces it with deny rules; codex and pi can't.
	Deny []string
}

// cwd is the directory the CLI runs in.
func (r Request) cwd() string {
	if r.Workspace != "" {
		return r.Workspace
	}
	return r.Dir
}

// Options customise how a backend's CLI is invoked.
type Options struct {
	Command string
	Args    []string
	Env     map[string]string
}

type Backend interface {
	Run(ctx context.Context, req Request) (string, error)
}

var registry = map[string]func(Options) Backend{
	"claude": func(o Options) Backend { return claude{o} },
	"codex":  func(o Options) Backend { return codex{o} },
	"pi":     func(o Options) Backend { return pi{o} },
}

func New(name string, o Options) (Backend, error) {
	build, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return build(o), nil
}

func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Command returns the executable used for a backend.
func Command(name string, o Options) string {
	if o.Command != "" {
		return o.Command
	}
	return name
}

// run executes the backend CLI with the prompt on stdin and returns its stdout.
// The panelist guard is always set, last, so configured env cannot clear it.
func run(ctx context.Context, name string, o Options, dir string, args []string, stdin string) ([]byte, error) {
	var env []string
	for _, kv := range os.Environ() {
		// A ripgrep config can set --pre, a command run on every file
		// searched; pi's (and likely claude's) grep honour it. Relative,
		// it would resolve inside the reviewed repository.
		if !strings.HasPrefix(kv, "RIPGREP_CONFIG_PATH=") {
			env = append(env, kv)
		}
	}
	for k, v := range o.Env {
		if k != "RIPGREP_CONFIG_PATH" {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, EnvGuard+"=1")
	return execCLI(ctx, Command(name, o), append(args, o.Args...), dir, env, stdin)
}

// execCLI runs a command with bounded output. so owns all three pipes, so
// waiting for the CLI to exit never also waits on helpers it left behind
// holding them. Once it exits, its process group is killed and the output is
// read to the end, with a grace period in case something outside the group
// still holds a pipe.
func execCLI(ctx context.Context, bin string, args []string, dir string, env []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	procgroup.Isolate(cmd)

	var pipes [6]*os.File // inR, inW, outR, outW, errR, errW
	for i := 0; i < 6; i += 2 {
		r, w, err := os.Pipe()
		if err != nil {
			for _, f := range pipes[:i] {
				f.Close()
			}
			return nil, err
		}
		pipes[i], pipes[i+1] = r, w
	}
	inR, inW, outR, outW, errR, errW := pipes[0], pipes[1], pipes[2], pipes[3], pipes[4], pipes[5]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW

	stdout := &headBuffer{limit: maxStdout}
	stderr := &tailBuffer{limit: maxStderr}
	done := make(chan struct{}, 3)
	go func() { io.Copy(stdout, outR); done <- struct{}{} }()
	go func() { io.Copy(stderr, errR); done <- struct{}{} }()
	go func() { io.WriteString(inW, stdin); inW.Close(); done <- struct{}{} }()

	err := cmd.Start()
	inR.Close() // the CLI holds its own copies
	outW.Close()
	errW.Close()
	if err == nil {
		err = cmd.Wait()
	}
	procgroup.Kill(cmd)

	// Normally all three finish at once. If something outside the group still
	// holds a pipe, close our ends; if even that does not unblock a read (it
	// may not on Windows), give up on it rather than hang. The buffers are
	// safe to read while a stray copy is still writing.
	if pending := waitFor(done, 3, 5*time.Second); pending > 0 {
		inW.Close()
		outR.Close()
		errR.Close()
		waitFor(done, pending, time.Second)
	}
	outR.Close()
	errR.Close()

	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return nil, ErrTimeout
			}
			return nil, ctxErr
		}
		cliErr := &CLIError{Bin: filepath.Base(bin), Err: err, Detail: tailBytes(tail(stderr.String(), 15), maxDetailBytes)}
		if cliErr.Detail == "" {
			cliErr.Detail = tailBytes(tail(stdout.String(), 15), maxDetailBytes)
			cliErr.FromStdout = cliErr.Detail != ""
		}
		// stdout is returned too: a CLI may report its error there (claude's
		// JSON result), which the caller can parse.
		return stdout.Bytes(), cliErr
	}
	if stdout.Truncated() {
		return nil, fmt.Errorf("%s: output exceeded %d bytes", filepath.Base(bin), maxStdout)
	}
	return stdout.Bytes(), nil
}

// CLIError is a CLI that failed, with the end of what it printed.
type CLIError struct {
	Bin    string
	Err    error
	Detail string
	// FromStdout means Detail came from stdout (stderr was empty), so it may
	// be the CLI's normal output rather than an error message.
	FromStdout bool
}

func (e *CLIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %v", e.Bin, e.Err)
	}
	return fmt.Sprintf("%s: %v\n%s", e.Bin, e.Err, e.Detail)
}

func (e *CLIError) Unwrap() error { return e.Err }

// maxDetailBytes bounds how much of a failing CLI's output goes into its
// error (and so into the run history): one long line could otherwise carry
// megabytes.
const maxDetailBytes = 4 << 10

// tailBytes keeps the last n bytes of s, starting at a character boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}

// waitFor waits for n signals on done, or until d passes, and returns how
// many are still outstanding.
func waitFor(done <-chan struct{}, n int, d time.Duration) int {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for ; n > 0; n-- {
		select {
		case <-done:
		case <-timer.C:
			return n
		}
	}
	return 0
}

// Output limits, so a runaway CLI cannot exhaust memory. Variables so tests
// can lower them.
var (
	maxStdout = 16 << 20
	maxStderr = 1 << 20
)

// tail returns the last n lines of s, trimmed.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
