---
title: Server runner
summary: How the server decides whether a PR makes docs stale with only an LLM key, and why it is built to resist prompt injection.
covers:
  - backend/internal/llm/**
  - backend/internal/agent/**
  - backend/internal/review/llmrunner/**
  - backend/internal/review/pipeline/**
  - backend/internal/review/instructions/**
  - backend/internal/github/files.go
  - backend/internal/review/basedocs/**
  - backend/internal/review/finalize/**
  - backend/internal/review/input/**
  - backend/internal/docs/candidates.go
  - backend/internal/review/diff.go
  - backend/internal/review/validate.go
---

# Server runner

The server runner is the analysis runner for repos without the pollux-agent Actions workflow. It is on when `LLM_PROVIDER` is set; see [Setup](../guides/setup.md). It implements the runner contract from `internal/review` and returns either "no impact" with a reason or a list of validated proposals.

The runner is a backend under the shared pipeline (`internal/review/pipeline`). The backend supplies the clone, the agent loop, and a triage-model judge; the pipeline owns selection, triage, the new-doc decision, verification, validation feedback, failure causes, limits, and usage, and imports neither `llm` nor `agent`. Each step below says what happens, whichever side runs it.

## Pipeline

1. **Changed files.** The gate lists the PR's files from GitHub, each with its patch and the head-side line ranges of its hunks, and passes them in the request. No changed files means "no impact" without cloning.
2. **Clone.** A depth-1 fetch of the PR head and its merge base, with head checked out; the base's `docs/` is read straight from git objects into memory, never checked out: a checkout would apply the PR's `.gitattributes` (encodings, line endings) to the base docs, and the agent's tools cannot reach them. Authenticated with the installation token. The token goes to git through its environment as an HTTP header, never in the remote URL, so it does not land in `.git/config` or in logs. `git` must be on the server's PATH.
3. **Candidate docs.** The runner parses `docs/` at the merge base (the base-branch commit the PR's diff starts from, resolved once by the gate through GitHub's compare API, not the base branch's current tip, so a doc added to main after the PR forked is not a candidate the head lacks) and keeps the docs whose `covers` globs there match a changed file or its old path (see [Architecture](../architecture.md) for the docs model). Matching at base is the point: a PR could otherwise empty or narrow `covers` to opt itself out, and editing `covers` is a doc change to review, not trust. The doc text still comes from head, so an edited doc is judged as the PR leaves it; a doc the PR adds is not a candidate, a renamed doc is followed to its new path (a doc renamed out of `docs/` or to a non-`.md` path counts as deleted; a file GitHub reports as copied is not a rename), and a candidate whose frontmatter the PR broke is triaged from its headings and text and can still get section proposals. A candidate the PR replaced with a symlink or other non-regular file at head, or a doc over 1 MiB at head, ends the analysis as a failure; so does a candidate under a symlinked directory at head, including `docs/` itself being a symlink (a symlinked subdirectory at base already failed the same way). A doc that fails to parse at base is skipped.

    The review input (merge base, docs to review, uncovered files, and every changed file with its commentable line ranges) is decided once from the PR's merge base, changed files, and base selection, independent of the runner; this runner renders it into its prompts and the Actions runner sends it in its dispatch. A failed clone ends neutral "Reading the repository failed."

    The same step computes the **uncovered files**: changed files that no doc covers at base, are not removed, and are not under `docs/`. Both runners share this calculation. With no candidate and no uncovered file (a removals-only or docs-only PR) the result is "no impact" without a model call.

    A candidate the PR deletes ends the run here, with no model call: the result is a restore proposal (a new-doc proposal carrying the doc's base content and its base index line) whose reason names the doc and the changed files it covered, with paths quoted. Apply brings it back, and the next push gets the full analysis. A covered change with no line diff (binary, pure rename, omitted patch) still gets the restore, anchored on any changed line in the PR. If every covered changed file is itself deleted, there is no restore: the feature and its doc were removed together. Deleting a covering doc on purpose needs Skip until merge.
4. **Triage.** One call per candidate doc to the triage model, given the PR's diff and the doc. Each answers impacted or not, with a reason. Then, if there are uncovered files, one more small-model call, given the docs index (`docs/README.md`, or a note that it exists but could not be read; empty when absent) and no tools, decides whether they need a new doc; it defaults to no. If every doc is "no" and no new doc is needed, the result is "no impact" carrying those reasons (for uncovered files, that no doc covers them and none is needed), and nothing further runs. Most PRs end here, which keeps them cheap. A mixed PR gets both: its candidates are triaged as always and its uncovered files still get the new-doc decision.
5. **Agent loop.** For impacted docs and/or a needed new doc, the main model reads the clone through tools and finishes by calling `submit_proposals`. Its arguments must pass proposal validation (path a `.md` or `.mdx` file under `docs/`, anchor on a file the PR changes and does not delete, on a line inside one of its hunks; the error lists the commentable ranges, see [Proposal output](proposal-output.md); the section name is normalized, so `## Usage` means `Usage`). A new doc may be proposed only when the new-doc decision said yes, it must follow the docs conventions (non-empty `title` and `summary`, relative links to other docs of the repo, in the doc and its index entry), its `covers` must match at least one uncovered file, so a new doc always documents code the PR changed, and its path must be free at head (a file of any size, a directory, a symlink, or a path under a file, symlink, or submodule all count as taken). A section edit needs its doc at head and a heading that exists exactly once; the problem lists the headings. These rules live in one shared finalize step that the Actions runner also runs on its artifact. More than 20 proposals in one submission is a problem by itself, so a submission cannot buy unbounded reads of the head. A submission with problems is not returned: the model is told all of them at once, each with the proposal's index, and may resubmit while steps remain.
6. **Verification.** One call per proposal asks whether it is right. Rejected proposals are dropped; if all are dropped, the result is "no impact".
7. **Section text.** The same finalize step attaches each proposal's section current text and head-side line range from the clone (empty for a new doc). Docs are parsed tolerantly, so a doc with broken frontmatter still yields its section text. The model never supplies them; the gate uses them to render comments (see [Proposal output](proposal-output.md)).

The diff comes from GitHub's per-file patches, not from git in the clone, so anchors agree with what GitHub shows. The clone is for reading docs and the code around the change.

## Why read-only tools rooted in the clone

PR content (code, comments, docs, commit messages) is attacker-controlled text that the model reads. Safety comes from what the model can do, not from asking it nicely. The only tools are `read_file`, `grep`, `list_dir`, and the finishing `submit_proposals` (`submit_docs` for a scaffold run): there is nothing to write, execute, or fetch, so an injected instruction has nothing to call. File access goes through `os.Root` on the clone, so `../`, absolute paths, and symlinks pointing out are refused by the OS-level check, not by string filtering. A refused call returns a tool error to the model and the run continues.

Everything taken from the PR (patch, doc text, the proposal under verification) reaches the model inside markers carrying a per-run random nonce, and every system prompt says text inside them is data. The draft and scaffold prompts wrap the rules in `internal/review/instructions` with server-only tool sentences; the Actions runner's prompts are generated from the same rules, so a rule change reaches both (see [Actions runner](actions-runner.md)). That lowers the odds of injection; it does not remove them, which is why the tools stay read-only and a human still applies every edit. Git itself runs on attacker-controlled content, so it gets a minimal environment: no server secrets, no system or global git config (so no host-configured filters or hooks), no terminal prompts, HTTPS only, and a token narrowed to the one repo with `contents: read`. Prompt inputs are capped (64 KiB per doc, 128 KiB of patch, with a visible truncation note) and more than 10 candidate docs is an analysis failure, so a PR cannot buy unbounded model spend. A file whose patch GitHub omits (large or binary) is shown to the model as omitted, not as an empty diff, and a renamed file matches docs covering its old path too.

The agent and LLM packages know nothing about GitHub or the review domain; the import boundary is enforced by depguard in `backend/.golangci.yml`.

## Limits

A run has a step cap, a token budget, and a deadline that leaves room inside the 3-minute target for GitHub calls. They are checked every step. Hitting any of them is an analysis failure, not a result: the error names which limit, and no partial proposals are returned. The gate ends the check neutral, titled "Analysis failed" (see [Proposal output](proposal-output.md)). The caps (12 steps, 120k tokens counting thinking tokens, 150 s) come from a Gemini free-tier spike where runs took 1–3 steps and under 50k tokens. A scaffold run reads the whole repo and is not on a PR's 3-minute path, so it gets 40 steps, 600k tokens, and 8 minutes. Three empty replies in a row (Gemini's malformed function calls) also end the run.

## Failure causes

The runner classifies every failure once, at the point it starts, into a fixed cause: provider error, timeout, step or token limit, too many candidate docs, clone failure, or internal. The gate shows fixed text per cause and anything unclassified as a generic failure. Provider errors and model replies can carry model output or response bodies, so the cause never quotes them; the detail stays in the server log.

## Run visibility

The agent loop logs one record per tool call (step, tool, the path or pattern truncated to 120 characters, tokens of that step; never file contents or submitted text) and one at the end with steps, tokens, duration, and outcome. Triage, new-doc, and verification calls log their tokens. Records carry `repo`, `pr`, and `head_sha` (scaffold runs `repo` and `scaffold_sha`), so one PR's run is a grep of the server log. Each analysis also ends with one "analysis done" record: repo, PR, head SHA, outcome, model, and token totals (a scaffold run: repo and `scaffold_sha`). The logger is passed to the pipeline and `agent.Task.Log`.

## Usage

The result reports the tokens of every model call in the analysis (triage, new-doc, the agent loop, verification), cache read and write tokens included where the Anthropic adapter returns them (the OpenAI-compatible adapter reports none). There is no cost: the runner has no pricing table. A result that called no model (a restore, no changed files, nothing to review) reports no usage. The result's model is the one that produced the verdict: the triage model when triage or verification ended the run, the main model when the agent loop did, none when no model ran.

## Size limits

Before any review LLM call or dispatch, the gate rejects a PR with more than 50 changed files or more than 1 MiB of patch text, for both runners; the check ends neutral "PR too large to analyze" naming the limit. A text file whose patch GitHub omitted (it reports changes but no patch) counts as over, since its size is unknown; a binary file has no patch and does not. Partial analysis of a large PR is not attempted. The file limit is one named constant so a per-plan value can replace it later; at 50 files the listing is far below GitHub's 3000-file cap, so a truncated listing cannot reach analysis.

A newer push cancels the running job; the context reaches every model call and git command.

## Providers

Two adapters speak the providers' HTTP wire formats: OpenAI-compatible chat completions (Gemini, OpenRouter, Ollama, and others) and Anthropic's Messages API. Free-tier models are weaker at tool use, so malformed tool arguments are returned to the model as errors instead of crashing the run. In the spike, half the proposals anchored on unchanged lines or named sections loosely, so the prompt shows the diff with the head-side line number printed on every context and added line, validation rejects an anchor outside the file's hunks with the commentable ranges in the error, and a section must name an existing heading; validation sends anything else back to the model. Gemini 3 attaches a thought signature to each tool call that must be sent back on the next turn, and counts thinking tokens only in the total.

Adapters must not trust the status code. GitHub Models was retired on 2026-07-30 and its endpoint now answers `200 text/plain "OK"`. A body that is not JSON, or has no `choices`/`content`, is an error.

Free tiers may train on inputs. Use them only against the sandbox repo.
