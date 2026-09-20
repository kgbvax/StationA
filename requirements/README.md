# Requirements queue

A file-based requirements → implementation → test → deploy loop, driven by
`/loop`. Each requirement is one markdown file; the loop picks up the oldest
file in `pending/`, works it end-to-end per the stationa conventions (root
`CLAUDE.md`), and moves it to `done/` or `failed/`.

## Layout

```
requirements/
  README.md      this file
  template.md    copy this to author a new requirement
  pending/       queue — the loop takes the lowest REQ number
  done/          finished, Outcome section filled in
  failed/        blocked — blocker documented in Outcome
  examples/      reference requirements (never picked up by the loop)
```

## Authoring a requirement

1. Copy `template.md` to `pending/REQ-<NNN>-<slug>.md` (NNN = next free
   number; numeric order is the processing order).
2. Fill in the frontmatter and sections. Keep requirements small — one loop
   iteration handles exactly one requirement end-to-end. If it cannot be
   implemented, tested and (where applicable) deployed in one sitting, split it.
3. Set `deploy:`:
   - `none` — tests green + branch is the deliverable, no deployment
   - `shari` — deploy to shari per `docs/conventions/deployment.md` after
     green tests
   - `manual-device` — loop builds the artifact and prints the sideload /
     flash command; a human does the physical step (tablet adb install,
     M5Stamp USB flash, …)

The doc is the contract: the loop implements what the acceptance criteria
say, nothing more. Write criteria verifiable by a test, a build, or an
observable MQTT/bus behavior.

## Running the loop

From the repo root, in the interactive session:

```
/loop 30m Work the requirements queue per requirements/README.md: take the
lowest-numbered REQ-*.md in requirements/pending/. If the queue is empty,
reply "queue empty" and do nothing else. Otherwise implement it, satisfy every
acceptance criterion, run the project's test gate (Go modules: go test ./...;
hf_console: tool/prebuild.sh), then handle deploy per the doc's frontmatter.
Work on a branch (never commit to main), move the doc to done/ or failed/
with the Outcome section filled in, and commit the doc move. One requirement
per iteration.
```

- Omit the interval (`/loop <prompt>`) to let the model self-pace between
  iterations; a fixed interval keeps the cadence predictable.
- Recurring loops auto-expire after 7 days — re-run `/loop` to restart.
- Deployments to shari are outward-facing actions. If you want a human gate
  before anything touches the station, add to the prompt: "For deploy: shari,
  stop after a green build and ask before deploying."

## Outcome section

The loop appends to the doc before moving it:

```
## Outcome
- date: 2026-09-18
- branch: feat/req-001-setup-screen
- commits: abc1234
- tests: tool/prebuild.sh — 87 pass
- deploy: built app-release.apk; sideload with adb install -r (manual)
- deviations: acceptance criterion 4 implemented as … (reason)
```
