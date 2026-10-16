// Package backend runs a single prompt through an agent CLI in headless mode.
//
// Every backend is invoked without sessions, MCP servers, or user config. By
// default it also has no tools and runs in an empty scratch directory, so the
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
func run(ctx context.Context, name string, o Options, dir string, args []string, stdin string) ([]byte, error) {
	bin := Command(name, o)
	cmd := exec.CommandContext(ctx, bin, append(args, o.Args...)...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range o.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Last so configured env cannot override it: exec uses the last value for a duplicate key.
	cmd.Env = append(cmd.Env, EnvGuard+"=1")
	cmd.Stdin = strings.NewReader(stdin)
	stdout := &headBuffer{limit: maxStdout}
	stderr := &tailBuffer{limit: maxStderr}
	isolateProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	// so owns the output pipes, so waiting for the CLI to exit does not also
	// wait for helpers it left behind holding them open. Once it exits, its
	// process group is killed and the output is read to the end.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	copied := make(chan struct{}, 2)
	drain := func(r *os.File, w io.Writer) {
		io.Copy(w, r)
		copied <- struct{}{}
	}
	go drain(outR, stdout)
	go drain(errR, stderr)

	err = cmd.Start()
	outW.Close() // the CLI holds its own copies
	errW.Close()
	if err == nil {
		err = cmd.Wait()
	}
	killProcessGroup(cmd)
	// Anything outside the group (a daemonised helper) could hold a pipe
	// open; stop reading after a grace period rather than hang.
	grace := time.After(5 * time.Second)
	for range 2 {
		select {
		case <-copied:
		case <-grace:
			outR.Close()
			errR.Close()
			<-copied
		}
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
		detail := tail(stderr.String(), 15)
		if detail == "" {
			detail = tail(stdout.String(), 15)
		}
		if detail != "" {
			return nil, fmt.Errorf("%s: %w\n%s", filepath.Base(bin), err, detail)
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(bin), err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("%s: output exceeded %d bytes", filepath.Base(bin), maxStdout)
	}
	return stdout.Bytes(), nil
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
