# PRD — Muehle station automation

This folder holds the product requirements for the Muehle amateur-radio station
automation system. A team with a different technology stack can use it to
rebuild the system. The team does not need access to the old code.

## How to read

Read the documents in this order:

1. `00-system-overview.md` — what the station is, what it holds, and what an
   operator does with it. Read this first.
2. `01-architecture.md` — the message bus, the slot model, the liveness model,
   and how the components work together.
3. `02-interface-spec.md` — the wire contract. Every slot, topic, payload, and
   command, with exact strings and values.
4. `03-components/*.md` — one behavior spec per component. Read the ones you
   must build.
5. `04-console.md` — the operator console (the tablet app).
6. `05-deployment-ops.md` — hosts, service conventions, and the dev tools.
7. `06-safety.md` — the never-happen rules. Read this before you build
   anything that switches power or routes radio-frequency energy.
8. `07-priorities-milestones.md` — the rebuild order and the acceptance gates.

Every document ends with a section "Open decisions and unresolved facts".
These items are real gaps in the sources. Milestone M0 in
`07-priorities-milestones.md` must close them before the first code starts.

## Reading rules

- When a PRD document and a file in `_research/` disagree, the PRD document
  wins.
- When a PRD document and the old code disagree on purpose, the PRD document
  says so. The old system has some defects. The PRD lists them as fix
  requirements, not as behavior to copy. `07-priorities-milestones.md` §4
  collects them.
- The reader does not know amateur radio. Every term has a plain-language
  definition at first use.
- Technology names (Go, Flutter, systemd, and others) appear only in passages
  marked as reference-implementation notes. They are background, not
  requirements.

## Folder map

| Path | Content. |
|---|---|
| `00-system-overview.md` | Purpose, physical inventory, operator view, scope, gaps. |
| `01-architecture.md` | Bus architecture, slots, planes, liveness, data flows. |
| `02-interface-spec.md` | The complete wire contract for every slot. |
| `03-components/` | One behavior spec per component (13 files). |
| `04-console.md` | Operator console feature spec. |
| `05-deployment-ops.md` | Hosts, deployment conventions, operations, dev tools. |
| `06-safety.md` | The safety specification and its honest gaps. |
| `07-priorities-milestones.md` | Priorities, milestones M0–M6, test and cutover strategy. |
| `_research/` | Informal deep-read notes from the old code (not STE, not binding). |
| `ste/` | The Simplified Technical English checker and its word lists. |

## Tooling

The documents use ASD-STE100 Simplified Technical English. To check them:

```bash
cd PRD/ste
python3 ste_check.py            # all documents
python3 ste_check.py 06-safety.md
```

A document must show 0 errors before it changes from draft status.