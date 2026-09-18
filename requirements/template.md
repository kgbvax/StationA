---
id: REQ-NNN
project: <component directory name, e.g. hf_console>
title: <one line>
created: YYYY-MM-DD
deploy: none | shari | manual-device
---

## Requirement

What and why, 2–5 sentences. Name the real files/slots involved. The doc is
the contract — the loop implements exactly this, so keep the scope tight.

## Acceptance criteria

- [ ] <verifiable statement — maps to a test, a build success, or an
      observable behavior on the bus>

## Constraints

Binding conventions, or "none". Typical ones: three-plane MQTT schema,
`value`-key cmd convention, config-in-0600-TOML, `tool/prebuild.sh` gate,
no credentials in the repo.

## Notes

Optional pointers: prior art, related docs, gotchas.

## Outcome

(filled in by the loop when this requirement is processed)
