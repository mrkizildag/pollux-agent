---
title: Docs scaffold
summary: How a repo with no docs/ folder gets one starting docs PR, exactly once, and what the waiting PRs' checks say meanwhile.
covers:
  - backend/internal/gate/scaffold.go
  - backend/internal/gate/sqlite/scaffold.go
  - backend/internal/github/scaffold.go
  - backend/internal/review/scaffold.go
  - backend/internal/review/pipeline/scaffold.go
  - backend/internal/review/pipeline/prompt.go
  - backend/internal/review/instructions/**
  - backend/cmd/genaction/**
  - backend/internal/review/actions/actions.go
  - action/run-claude.sh
  - backend/internal/docs/doc.go
  - backend/internal/gate/gate.go
  - backend/internal/httpapi/handler.go
  - backend/internal/httpapi/job.go
  - action/action.yml
  - action/scaffold.md
  - action/scaffold.schema.json
---

# Docs scaffold

A PR whose head has no `docs` has nothing to analyze. It gets a neutral check titled "No docs/ folder" and no analysis. pollux then writes a starting `docs/` from the repo's code and opens one PR with it. With no analysis runner configured nothing can write it, so the summary says a runner is needed and links the [setup guide](../guides/setup.md).

## One scaffold per repo, ever

The scaffold is repo-keyed state, not PR state. A row per repo in SQLite moves through phases: idle, writing (or awaiting, for the Actions runner), written, opened. `opened` is terminal: once a scaffold PR has been opened there is never a second one, even after the first is closed unmerged or merged. A maintainer who closes it has answered the question.

The work runs as a scaffold job keyed by repo, so a repo's scaffold jobs are serialized and kept apart from its PR jobs. Every PR that finds no `docs` registers as waiting and enqueues the job. When the PR opens, every waiting PR's check is updated with its link, and every later PR event on an opened scaffold relinks any waiter not yet linked. A PR with "Skip this PR" in effect keeps its skip check and does not request a scaffold.

`docs` existing as anything (folder, file, submodule) counts as present; only a missing `docs` triggers the scaffold.

Before writing, the job probes the default branch. If it already has `docs/` (the PR's base was just stale), nothing is written and the waiting checks say to merge or rebase.

## Writing

The writer is chosen like the review runner, see [Architecture](../architecture.md).

- **Actions workflow repos**: the gate dispatches the existing workflow with `pr_number` "0" and `head_sha` set to the default branch tip. The action treats PR number 0 as scaffold mode and writes the files instead of reviewing a diff. It also sends `docs` with empty `review` and `uncovered` lists (scaffold mode ignores it), since the workflow requires that input for reviews (see [Actions runner](actions-runner.md)). Installed workflows reference `action@main`, so the action change must reach `main` before the server that dispatches scaffolds is deployed.
- **Server runner**: the LLM agent runs over a clone of the default branch with the same read-only tools as review, confined to the clone, and with larger step, token, and time limits since it reads the whole repo.

The output is exactly three files: `docs/README.md`, `docs/architecture.md`, `docs/guides/setup.md`. Each follows the new-doc convention: a non-empty `title` and `summary`, a `covers` key in its frontmatter, and relative links to other docs of the repo (no `/docs/...` paths or GitHub URLs to its docs). The index must link the other two. The result type has no field for any other path, so a model cannot write outside these files.

## Opening the PR

The branch `pollux-agent/docs-scaffold` is created off the default tip, with one commit and one PR into the default branch. Before touching the branch, pollux looks up PRs from it: one opened by the pollux bot (matched by its exact `<app-slug>[bot]` login) in any state, open, closed, or merged, is adopted, so no second scaffold PR is ever opened for the repo. An open PR from anyone else fails the attempt without changing anything; otherwise a branch at another commit is force-reset to the default tip before committing. The validated files are stored as soon as they are written, so a failed commit or PR step retries without running the model again. A crash after the branch or PR exists is recovered by adopting them, not creating duplicates.

Any failure before the PR exists, including a workflow run that fails, is cancelled, or passes its deadline, leaves the repo's state retryable. Waiting checks then say an attempt failed, and the next PR event on a docs-less PR tries again. After 3 failed attempts the scaffold stops for good and the checks say it could not be written, so `docs/` has to be added by hand.

## Permissions

Creating the branch and the PR uses the App's Contents write and Pull requests write permissions, see [Registering the GitHub App](../guides/github-app.md).
