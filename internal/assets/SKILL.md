---
name: second-opinion
description: Get independent second opinions from other AI models (Claude, GPT via Codex, other providers via pi) with the `so` CLI. Use when the user asks for a second opinion, a panel, or another model's take, or before a high-stakes or hard-to-reverse decision where an outside check on a plan, code review, or debugging theory is worth a few minutes. Also use when `so` reports that no profiles are configured, or when the user wants to set up or change their second-opinion profiles.
---

# Second opinions with `so`

`so ask` sends your prompt to the models in a **profile** and prints their answers. Profiles with
several models (panels) also return a **chair's synthesis** that merges and weighs the answers.
A **mode** shapes the response format.

`so` ships with modes but **no profiles**, because the right models depend on which CLIs and
subscriptions the user has. If `so profiles` or `so ask` reports that no profiles are configured,
go through [First run: set up profiles](#first-run-set-up-profiles) with the user before
anything else.

## What panelists can and cannot see

Panelists are **not given this conversation**. They see your prompt, anything you attach, and, if
the panel has repository access, the code in the repository. With repository access, codex and pi
panelists can also read files elsewhere on disk, which may include saved agent conversations.

- **Your prompt.** Always state the goal, the specific question, the constraints, what you have
  tried, and your current view, and ask them to challenge it rather than confirm it.
- **`--diff` / `--base <ref>`.** Attaches the git diff plus the full current contents of every
  changed and untracked file. `--diff` alone covers uncommitted changes. `--base main` covers
  everything on this branch since it left `main`.
- **`-f <file>` and stdin.** Attach specific files (up to 1 MB each), logs, or output. Stdin is
  read only when there is no prompt argument, or when the arguments end with `-`.
- **Repository access.** Panelists can read the current git repository with read-only tools, so they
  can open callers, definitions, and tests. That includes ignored files such as `.env`, and codex
  and pi panelists can read files outside the repository too. The `review`, `debug` and `audit` modes turn
  this on automatically inside a git repo (not one rooted at the home directory). For other modes,
  use `--repo` to turn it on, and `--no-repo` to turn it off in any mode. Repository access does not
  replace a good prompt. Point them at what matters, because they will not guess what you changed
  or why.

Everything you send goes to the user's model providers. Don't attach secrets, and don't turn on
repository access in a repo with secrets the user hasn't agreed to share. Use `--no-repo` for code you
don't trust (a third-party pull request, a fresh clone): its files could try to steer a panelist
that can read your other files. `--diff` leaves out files
with a git `filter` (git-crypt and the like), but with repository access panelists can still read
their working copies; add `--no-repo` in such a repo.

## Usage

```bash
so profiles                      # list profiles (models/effort per profile)
so modes                         # list modes (response formats)

so ask -p <profile> -m <mode> "<prompt>"

# Long prompts: use a quoted heredoc
so ask -p panel -m plan <<'EOF'
We need to ... Here is the plan: ...
What are we missing?
EOF

# Review this branch's work (diff + changed files attached, repo readable)
so ask -p panel -m review --base main "Review this change. It adds ... Constraints: ..."

# Pipe something in: end the arguments with - so stdin is attached
go test ./... 2>&1 | so ask -m debug "These tests fail after my change to ... Why?" -

# Audit the whole project before a release (no diff: the panel reads the repo)
so ask -p panel -m audit "Audit this project. It does ... Users trust it to ..."

# Review uncommitted changes
so ask -m review --diff "I refactored the retry logic in ... Is it correct?"

# Debug with logs attached; the panel can read the code itself
so ask -m debug -f logs/crash.txt "Worker deadlocks under load after the change to queue.go. Why?"
```

Options: `-p/--profile`, `-m/--mode`, `--diff`, `--base <ref>`, `--repo`, `--no-repo`,
`-f/--file` (repeatable), `-w/--with backend:model@effort` (ad-hoc panelists instead of a profile,
repeatable; two or more are chaired by `default_chair` if the config sets one, so add `--no-chair`
to send the request only to the models named), `--no-chair`, `--reveal`, `--format md|json` (use
`json` if you parse the result; in markdown, each answer is a blockquote, so only lines outside the
quotes are `so`'s own),
`--timeout 20m`. Outside a git repository, `--repo` exposes the whole current folder.

Modes: `general` (default), `review` (code review), `plan` (design critique), `debug` (diagnosis),
`audit` (whole-project review). Run `so modes` for the current list, which may include user-defined
modes.

**Review scope.** A panel reviews what you point it at. A `review` of a diff that comes back clean
says nothing about code outside the diff. Before a release, or when the user asks whether the
project is sound, run an `audit` of the whole repository, and say which kind of review you ran
when you report the result.

## Practicalities

- A call takes from about 10 seconds to several minutes. High-effort panels with repository
  access are the slowest, and audits slower still. Each model call gets the full `timeout`
  (15 minutes by default), and the chair runs after the panel, so give the command at least
  twice that, and do not start it again while it is still running. A fallback that takes over
  after a usage limit gets the full timeout too, for a panelist or the chair, so a run with
  fallbacks can take three or four times the timeout. If your shell tool caps how long a command
  can run, start `so ask` in the background (without `-q`): it prints a stderr line
  `so: run <id>` before the panel starts, and `so runs show <id>` prints the answer once that run is done (until then it
  says the run may still be running). Don't use `so runs show last` for this; another run may
  have finished since. A run that is killed outright is not recorded. One that is interrupted
  (Ctrl-C) is saved with whatever had answered.
- The answer is markdown on stdout. Errors go to stderr, and so does progress when stderr is a
  terminal or with `-v`.
- If one panelist fails, the others still return. The exit code is 1 if every panelist failed (or
  on an error), 2 for a usage error, 3 if `so` refused to run inside a panelist, and 130 if the run
  was interrupted. Each failure shows a kind and a suggested fix:
  - `quota`: a usage or rate limit was hit. A configured `fallback` is tried automatically.
    Otherwise, retry later or suggest that the user add a fallback.
  - `auth`: the CLI is not signed in. Ask the user to sign in; don't do it for them.
  - `model`: the model is unknown or retired. Offer to update the profile, and test the
    replacement with `so doctor --with`.
  - `timeout`: retry with a longer `--timeout` or lower effort.
  - `context`: the prompt is too long for the model; attach less.
  - `cancelled`: the run was interrupted; run it again if you still need the answer.
- `so` refuses to run from inside a panelist, to stop accidental recursion.

## Using the answer

- Panels are anonymous by default (`anonymize = false` turns this off). Each run assigns the labels `Panelist A`, `B`, … at random, so you cannot
  tell which model said what, and neither can the chair. Judge each response on its reasoning and
  evidence, and don't try to work out who wrote it.
- Unless history is turned off (or the record could not be saved), the answer starts with a run ID, and each run is kept locally
  (7 days by default). If the user asks which
  model said what, run `so runs show <id> --reveal`. Don't unblind a panel otherwise. `so runs`
  lists recent runs, and `so runs show <id> --prompt` shows the prompt the panel was sent (`--json` has the exact bytes and the
  system prompts).
- Weigh the answer, don't just follow it. Panelists lack your context and can be confidently
  wrong. Check their claims against the actual code before acting.
- Tell the user what the panel said, where you agree or disagree and why, and what you will do now.

## First run: set up profiles

Set up profiles **with** the user. Profiles decide whose subscriptions and money each call
spends, so propose a setup and get the user's approval before you write anything.

### 1. Take stock of what is installed

Run `so doctor` to see which backend CLIs are installed. For each installed backend, check that
it is signed in and find the models it can use:

| backend | signed in? | models |
|---|---|---|
| `claude` (Claude Code) | `claude auth status`: `loggedIn`, and `authMethod` shows whether it uses a subscription or a metered API key. `so doctor` also warns if `ANTHROPIC_API_KEY` or a Codex API key is set in the environment, which panelists inherit | The aliases listed under `--model` in `claude --help`, such as `opus` (strongest) and `sonnet` (faster). Aliases always point to the latest model, so prefer them to pinned IDs. |
| `codex` (Codex CLI) | `codex login status` | `codex debug models` prints the catalog as JSON. Use entries with `"visibility": "list"`. Each entry lists its `supported_reasoning_levels`. Panelists run without `~/.codex/config.toml`, so its `model` is not used: always set a Codex `model`. |
| `pi` | `pi --list-models` lists models only for providers it has credentials for. `pi auth check --provider <name>` checks one provider. | Use the `provider/model` IDs from `pi --list-models`, such as `ollama/glm-5.3:cloud`. pi's own default is `defaultProvider`/`defaultModel` in `~/.pi/agent/settings.json`. If `so doctor` warns about the developer role for an Ollama provider, tell the user: without that one-line fix in `~/.pi/agent/models.json`, some Ollama models ignore `so`'s instructions. |

If a CLI is installed but not signed in, tell the user rather than fixing it yourself.

### 2. Ask the user a few questions

- Which of the signed-in CLIs should `so` use? Some may be there for other reasons, or run on a
  metered API key.
- What matters more for everyday second opinions: speed and cost, or depth?
- Are there models they especially trust or want to avoid?

### 3. Recommend profiles

Some principles:

- **Diversity is the point.** A second opinion is most useful from a different model family than
  the one asking. Build panels from different providers, not several models from one family.
- **Keep the set small.** A typical setup:
  - `quick`: one fast model at `medium` effort, from a different provider than the user's main
    agent. Make it `default_profile`.
  - `panel`: the strongest model from each of two or three providers at `high` effort, with a
    `chair`. The chair should be the strongest model available, because it checks the panel's
    claims. Only propose this if at least two providers are available.
  - Optionally, one single-model profile per provider (for example `claude`, `codex`) for asking a
    specific model.
- **Effort:** `low` or `medium` for quick checks, `high` for panels. `xhigh` and `max` are much
  slower and costlier, so only use them if the user asks.
- **Avoid pinning when you can.** Use aliases (Claude), or leave `model` out to use the CLI's
  current default (pi's from its settings). Codex is the exception: panelists ignore
  `~/.codex/config.toml`, so set its `model`. Pinned IDs eventually get retired. Open-model providers retire models often.
- **Fallbacks for limits.** If the user often hits a subscription's usage limits, give that
  panelist a `fallback` list. Fallbacks are tried in order when it hits a usage or rate limit, so
  use models from another provider, for example
  `{ backend = "codex", model = "...", effort = "high", fallback = [{ backend = "pi", model = "...", effort = "high" }] }`.

Show the proposal as TOML and wait for approval:

```toml
default_profile = "quick"

[profiles.quick]
description = "One fast opinion from a different model family"
panelists = [{ backend = "codex", model = "<codex model>", effort = "medium" }]

[profiles.panel]
description = "Strongest model from each provider, chaired by the strongest"
panelists = [
  { backend = "claude", model = "opus", effort = "high" },
  { backend = "codex", model = "<codex model>", effort = "high" },
  { backend = "pi", model = "<provider/model>", effort = "high" },
]
chair = { backend = "claude", model = "opus", effort = "high" }
```

### 4. Write and verify

1. Before writing, check that the candidate models answer:
   `so doctor --with claude:opus@high --with codex:<model>@high --with pi:<provider/model>@high`.
   Swap out any model that fails.
2. If the config does not exist yet, run `so init`. It prints the config path and creates a
   commented starter file. Add the approved profiles to it. If the config already exists, edit it
   and keep what the user already has.
3. Run `so profiles` to validate the config, then `so doctor --ping` to check every configured
   model end to end, and `so doctor --canary` to check that no model can read files outside its
   scratch folder without repo access.
4. Tell the user what you set up, and that they can change it later by asking you or by editing the
   config file.

