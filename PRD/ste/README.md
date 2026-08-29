# STE tooling for the stationa PRDs

This directory holds the ASD-STE100 (Simplified Technical English, Issue 9,
https://www.asd-ste100.org) tooling for the PRD document set.

## Why not the official dictionary?

The full ASD-STE100 dictionary (about 875 approved / 1274 non-approved words)
carries an ASD copyright, so the repository cannot copy it. We use:

- `dictionaries/` — a small MIT-licensed curated base
  (github.com/swarooppatilx/ste100), extended with our own non-approved/approved
  pairs (well-known and documented public examples) and pruned of the base
  project's two errors (`can` and `will` ARE approved in STE).
- `dictionaries/project.json` — **our own extended vocabulary** (STE rules
  1.5/1.8/1.12): technical nouns and verbs specific to the Mühle station domain
  (broker, topic, payload, retained message, bridge, slot, rotator, tuner,
  component names, MQTT/QoS/LWT, feeder terms, and so on).
- `dictionaries/exemptions.json` — words the generic list rejects but the
  domain deliberately uses, with a written reason (rule 1.6 allows unapproved
  words as technical nouns).

The checker therefore proves a useful subset, not full conformance. A writer
or editor must still apply the human-only rules (rule 9.1: reword when
replacement is not enough).

## Checker

```bash
cd PRD/ste
python3 ste_check.py            # all PRD docs (excludes _research/)
python3 ste_check.py --summary  # per-file counts only
python3 ste_check.py 06-safety.md
```

Exit status is 1 when any ERROR-level finding remains (command for CI).
WARN-level findings are judgement calls. Passive voice is legal in descriptive
writing when the agent is unknown (rule 3.6). Descriptive sentences can have
up to 25 words (rule 6.3). Procedural sentences are max 20 words (rule 5.1).

## Writing rules the checker cannot check

Follow the writing SKILL in `.claude/skills/ste-english/SKILL.md` — it
summarizes the STE rules that need a human (one topic per paragraph, articles
before nouns, verb as noun avoidance, active voice in procedures, and so on).