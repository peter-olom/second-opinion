# second-opinion

`so` is a single CLI for getting second opinions from other AI models. It works through the agent CLIs
you already have, so each call uses your existing subscriptions instead of API keys:

| backend  | CLI           | typical subscription          |
|----------|---------------|-------------------------------|
| `claude` | Claude Code   | Claude                        |
| `codex`  | Codex CLI     | ChatGPT                       |
| `pi`     | pi            | Ollama Cloud (or any pi provider) |

You can call it from any harness. An agent runs `so ask`, the prompt goes out to a **profile** (one model,
or a panel answering in parallel), and a **chair** model synthesizes the panel's answers. A **mode**
fixes the response structure, for example a code-review format.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/peter-olom/second-opinion/main/install.sh | sh
so skill install          # teach Claude Code, Codex, and pi to use so
```

Then ask your agent for a second opinion. On first use it will set up profiles with you (see
[Profiles](#profiles)).

The script downloads the release for your OS and architecture, verifies its checksum, and installs
to `~/.local/bin`. You can set `SO_VERSION=v0.1.0` to pin a release, or `SO_INSTALL_DIR` to install
somewhere else. Windows builds are available as zips on the
[releases page](https://github.com/peter-olom/second-opinion/releases).

`so skill install` writes the agent skill into the skills directory of every harness it finds:
`~/.claude/skills`, `~/.codex/skills`, and `~/.pi/agent/skills`. Name harnesses to limit it
(`so skill install claude`), or add `--dir <path>` for other skill directories. Re-run it after
upgrading `so` to refresh the skill.

### Uninstall

```bash
so uninstall          # removes the skill files, run history, and the so binary; keeps your config
so uninstall --all    # also removes the config file in use (--config or $SECOND_OPINION_CONFIG)
```

It lists what it will remove and asks before deleting (`--yes` skips the prompt). Some details:

- It deletes only files `so` created: each `SKILL.md`, the `runs/*.json` files, the binary, and
  with `--all` the config file. Folders are removed only if that leaves them empty. Anything you
  added stays, and so does a config that shares a folder with the history.
- It leaves symlinked folders alone, and never removes a folder that one of them points to, so no
  link is left dangling. To be safe, with `--all` it also keeps the config's folder (even if empty)
  when the config path is relative, contains `.` or `..`, or when the config's own folder is a
  symlink.
- If you run `so` through a symlink, only the link is removed. The file it points to stays. If you
  run the binary itself, any `so` links on your PATH that point to it are removed too, so none is
  left dangling (not on Windows, where the binary is moved aside instead). If it can't prove which file you ran, it leaves the binary in place and tells you.
- On Windows a running program can't delete itself, so the binary is renamed to `so.exe.old` for
  you to delete afterwards.
- If any folder it would remove from can't be inspected, it stops before deleting anything. If it
  can't inspect the binary, it keeps it and tells you.
- On Windows, if `so.exe.old` already exists it uses `so.exe.old.1`, and so on, rather than
  overwriting it.
- It doesn't track skills you installed elsewhere with `so skill install --dir`, so remove those
  yourself. It also tells you if another `so` is still on your PATH.

## Usage

```bash
so ask "Should this cache be write-through or write-back given ...?"
so ask -p panel -m plan -f docs/design.md "Critique this design."
so ask -p panel -m review --base main "Review this branch. Context: ..."
so ask -m review --diff "Review my uncommitted changes to ..."
so ask -w claude:opus@high -w pi:ollama/kimi-k2.7-code:cloud@high "..."   # ad-hoc panel
so ask -p panel --format json "..."                                        # machine-readable
```

Progress goes to stderr and the result to stdout. If some panelists fail, the run still succeeds with
the ones that answered. The exit code is non-zero only when every panelist failed.

## Anonymous panels

Model names can bias the reader. A chair might favour its own model family, and a calling agent
might trust a "bigger" model's verdict over a sound argument from a smaller one. Panels are
therefore blind by default:

- Each run labels panelists `Panelist A`, `B`, … with letters shuffled at random, so a letter or
  position doesn't tell you which model it is. (With a single panelist, it is always `Panelist A`.)
  The chair is shown as `Chair`.
- The chair sees only these labels. Panelists are told not to identify themselves, and the chair is
  told not to guess who wrote what.
- The output (markdown and JSON) and the progress lines use the same labels.
- `so ask --reveal` appends a **Panel key** that maps letters to models, after the fact. The chair
  stays blind even then.
- Every run is recorded (unless you set `history = false`), so you can unblind a panel later (see
  [Run history](#run-history)). A failed panelist's error names its backend, so anonymous output
  shows only the error's kind; `so runs show <id> --reveal` shows the full error.
- Set `anonymize = false` in the config to use model names everywhere instead.

## When a model fails

The other panelists still answer, and the exit code is non-zero only when every one fails. Each
failure gets a category and a suggested fix in the output (`error_kind` in JSON):

| kind | meaning | what to do |
|---|---|---|
| `quota` | usage or rate limit, or the provider is overloaded | wait, or add a `fallback`. A quota error triggers the fallback automatically |
| `auth` | the CLI is not signed in, or its credentials were rejected | sign in to that CLI |
| `model` | unknown, retired, or unsupported model | update the profile and test with `so doctor --with` |
| `timeout` | did not finish within `--timeout` | longer timeout or lower effort |
| `context` | the prompt is too long for the model | attach less: a narrower `--base`, fewer `-f` files |

The CLIs report errors as free text, so `so` sorts them by matching common phrasings and HTTP status
codes. Anything it doesn't recognise is `other`. When a fallback answers, the output says so
without naming either model (for example, "answered by fallback after 1 usage limit").
`--reveal` and `so runs show --reveal` show which models were involved.

## Run history

Each run is saved privately to `~/.local/state/second-opinion/runs/<run-id>.json`, or under
`$XDG_STATE_HOME`. Files are `0600` and the folder is `0700`. A record holds the prompt, the system
prompts sent, every answer and error, timings, and which model was which panelist. The run ID
appears at the top of every answer.

```bash
so runs                          # recent runs: ID, profile, mode, how many answered, prompt
so runs show last                # reprint a run (still anonymous)
so runs show a3f9 --reveal       # unblind it: append the panel key (any unique part of the ID works)
so runs show a3f9 --prompt       # include the exact prompt the panel received
so runs show a3f9 --json         # the full record
```

Runs older than 7 days are pruned whenever a new run is saved. Change that with `history_days`, or
turn recording off with `history = false`. Records contain whatever you sent the panel, including
code and diffs, so treat the folder accordingly.

## What panelists can see

The caller always writes the prompt. There are two ways to give the panel more code to look at:

- **`--diff` / `--base <ref>`** attaches the git diff and the full current contents of every changed
  and untracked file. `--diff` covers uncommitted changes. `--base main` covers everything since the
  branch left `main`, via the merge-base, so `main`'s later commits are excluded. Only regular files
  are attached: symlinks are never followed, and binary, very large, or special files are listed but
  not attached. Git's textconv and external diff drivers are off, and files with a git `filter` in
  `.gitattributes` (git-crypt, transcrypt, sops-style filters, LFS, …) are left out of both the diff
  and the attachments, since their working copies may be decrypted. If there are too many changed
  files to check this, `--diff` stops rather than send a partial check. Untracked files are attached
  unless git ignores them, so add anything other tools leave in your working tree to `.gitignore` or
  your global excludes. Limits: 1 MB of diff, 256 KB per file, 1.5 MB of files in total.
- **`-f <file>` and stdin.** `-f` attaches a regular file of up to 1 MB. Stdin (up to 2 MB) is read
  only when there is no prompt argument, or when the arguments end with `-`:
  `git diff | so ask -m review "Review this" -`. If stdin is redirected from a file but not read,
  `so` says so. The whole prompt is capped at 6 MB.
- **Repository access** runs the panel and the chair in the repository root with read-only tools,
  so they can read callers, definitions, and tests, including ignored files such as `.env`. The
  `review`, `debug` and `audit` modes enable it by default (`repo = true` in the mode), but only inside a git
  repository that isn't rooted at your home directory (a dotfiles repo in `~` doesn't count).
  `--repo` turns it on for any mode and `--no-repo` turns it off. Inside a home-rooted repository,
  `--repo` gives the panel the current folder rather than all of `$HOME`, and refuses in `$HOME`
  itself.

Everything you send goes to the model providers in your profiles. Treat a prompt, its attachments,
and (with repository access) whatever the panel reads as shared with them.

## How models are invoked

Each panelist runs headless with no MCP servers and no saved session, in an empty temporary
directory (or the repository root, with repo access). No backend can modify files. What each one can
reach differs, and these are the tested guarantees:

| backend | without repo access | with repo access |
|---|---|---|
| claude | No tools at all (`--tools ""`). | `Read`, `Grep`, `Glob` only. Reads outside the repository are denied. |
| codex | Every Codex feature except networking is disabled, including **connected apps** (Gmail, GitHub, Jira, …), browser and computer use, sub-agents, image generation, and web search; so are its shell tools. Codex has no switch that guarantees no tools at all, so a read-only command path may occasionally remain. | The same, but with read-only commands enabled so it can explore the repository. Those can read any file you can read, not just the repository. Codex also loads the repository's `AGENTS.md`. |
| pi | No tools (`--no-tools`) and no context files. | `read`, `grep`, `find`, `ls`, which are not confined to the repository. pi also loads `AGENTS.md` context files from the repository, its parent folders, and `~/.pi/agent`. |

Claude also gets `--setting-sources "" --strict-mcp-config` (your `CLAUDE.md` memory files may still
load), codex gets `--ignore-user-config --ignore-rules`, and pi gets `--no-extensions --no-skills
--no-approve` (your pi settings, such as the default model, still apply). Every panelist is told not
to use tools without repo access.

The Codex lockdown asks Codex for its feature list (`codex features list`), which reads your
`~/.codex/config.toml`. If that fails, Codex panelists stop with an error rather than run with
their tools unlocked.

A reviewed repository's own `.codex/config.toml` (MCP servers, features) and pi files under
`.pi/` (extensions, settings) are not loaded: in testing, an MCP server and an extension planted in
a repository did not run while a panel reviewed it.

pi migrates files in its working directory when it starts. The one project-level migration in pi
1.3 renames `.pi/commands/` to `.pi/prompts/`. In a repository where that would happen, `so` doesn't
run the pi panelist at all, and it reports why. A later pi version could add migrations of its own.

These flags assume no overriding `args` in a `[backends.*]` section of your config: those are
appended after `so`'s own flags, so they can undo the lockdown.

`so doctor --canary` tests this on your machine. It plants a file outside each model's scratch
folder and asks the model, configured exactly as a panelist without repo access but explicitly
invited to use any tool it has, to read it. A ✓ means it did not read the file on that run, which is
evidence rather than proof. In testing, a Codex panelist with this lockdown didn't read the file in
10 of 10 runs, while the same check caught an unlocked Codex every time.

`so` sets `SECOND_OPINION_DEPTH=1` for every panelist and refuses to start a panel, ping models, or
uninstall when that variable is present. This stops accidental recursion; it is cooperative, so a
panelist with a shell could get around it.

Each CLI runs in its own process group, which is killed after the CLI exits (taking any helpers with
it) and when `so` is interrupted or its terminal closes (unless started with `nohup`). On Linux, if
`so` itself is killed outright, the CLI process it started dies too, but a process that CLI started
(npm-installed CLIs run a native binary under Node) can keep running. On macOS, `kill -9` of `so`
can leave a panelist running. On Windows, only the CLI
process itself is stopped, and the backends are less tested (npm-installed CLIs run through `.cmd`
shims, which may mangle multi-line arguments).

Each model call gets the full timeout. The chair starts after the panel finishes, and each fallback
gets its own timeout too, so a run can take about twice the timeout or more. Panelists with repo
access and a shell (codex, pi) can read your run history in `~/.local/state/second-opinion`.

## Configuration

The config lives at `~/.config/second-opinion/config.toml`. You can point to a different file
with `SECOND_OPINION_CONFIG` or `--config`.

### Profiles

**`so` ships no profiles.** The right models depend on which CLIs and subscriptions you have. On
first run, `so ask` reports that no profiles are configured. At that point the agent skill
walks your agent through setup with you:

1. It checks which CLIs are installed and signed in, and which models each can use.
2. It asks which subscriptions to use, and whether you care more about speed or depth.
3. It proposes profiles, favouring panels drawn from different providers.
4. Once you approve, it writes them and checks each model responds.

You can also write them yourself. `so init` creates a commented starter file.

```toml
default_profile = "quick"   # optional when there is only one profile

[profiles.quick]
description = "One fast opinion"
panelists = [{ backend = "codex", model = "gpt-6.1-sol", effort = "medium" }]

[profiles.panel]
description = "Strongest model from each provider, chaired by Claude"
panelists = [
  { backend = "claude", model = "opus", effort = "high" },
  { backend = "codex", model = "gpt-6.1-sol", effort = "high" },
  { backend = "pi", model = "ollama/glm-5.3:cloud", effort = "high" },
]
chair = { backend = "claude", model = "opus", effort = "high" }
mode = "review"   # optional: the profile's default mode
```

A panelist or chair can list **fallbacks**. These are tried in order when it hits a usage or rate
limit:

```toml
panelists = [
  { backend = "codex", model = "gpt-6.1-sol", effort = "high",
    fallback = [{ backend = "pi", model = "ollama/glm-5.3:cloud", effort = "high" }] },
]
```

A fallback can be a model already on the panel; the panel then has two seats for it. Prefer
fallbacks from a provider not already in that profile.

If you leave out `model`, the CLI uses its own default. `effort` is passed to the CLI's effort or
thinking setting. Claude and Codex accept `low` to `max`, and pi also accepts `off` and `minimal`.

```bash
so profiles                                   # list profiles (validates the config)
so doctor --ping                              # ping every configured model
so doctor --with pi:ollama/kimi-k2.7-code:cloud@high   # try a model before adding it
```

### Modes

Modes ship inside the binary (`internal/assets/modes.toml`) and update with it:

| mode | for | repo access |
|---|---|---|
| `general` | questions, decisions, ideas | no |
| `review` | code review: verdict, severity-ranked findings (R1…) with failure scenario, evidence and fix, then test gaps, questions and coverage. The chair merges these into confirmed or plausible findings (F1…), lists rejected findings, and ends with a next-steps checklist | yes |
| `plan` | critique of a design or plan before building it | no |
| `debug` | ranked root-cause hypotheses with how to confirm each one | yes |
| `audit` | whole-project audit: what leaves the machine, promises the code doesn't keep, risky paths, untested code. Same findings structure as `review` | yes |

A mode has `instructions` and a `format` for panelists, plus optional `chair_instructions` and
`chair_format` for the chair, and `repo` for default repository access. To customise a mode,
override it in your config. Overrides apply **field by field**, so you set only what you want to
change and the rest keeps tracking the built-in:

```toml
[modes.review]
format = """
## Verdict
...
"""
```

If you change a panelist `format`, check whether the chair's `chair_format` still fits it.

```bash
so modes                  # lists modes, marking [customized] built-ins and [custom] modes
so modes review           # the exact system prompts panelists and the chair receive
so modes review --toml    # the full mode as TOML, to paste into your config and edit
```

You define a new mode the same way:

```toml
[modes.security]
description = "Security audit of a change"
repo = true
instructions = "Audit for injection, authz gaps, secret handling, and unsafe deserialization."
format = "..."
```

## Development

```bash
make build      # ./bin/so
make install    # ~/.local/bin/so from this checkout
make test       # go vet + go test
make snapshot   # build all release archives into dist/ without publishing (needs goreleaser)
```

CI runs `make test` (with the race detector) and `make build` on Linux and macOS for every push
and pull request. A Windows job runs `go vet`, builds `so`, and runs the Windows-specific path and
uninstall tests. A snapshot build keeps the release config working. To release, push a tag:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

The release workflow runs the tests, then `make release`. GoReleaser builds darwin, linux, and
windows binaries for amd64 and arm64 and publishes them with `checksums.txt` as a GitHub release.
