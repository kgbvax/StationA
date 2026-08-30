# powerseq

The station startup/shutdown **sequencer** for the Mühle station automation
ecosystem — a logic slot (no device) implementing the integration-model
`sequencer` role at `muehle/hf/power-seq`. It subscribes the `/status` of every
slot its sequence references (and the `/state` of every `wait_state` target) and,
on the operator one-button `/cmd` (`start`/`stop`, not retained), runs an ordered
sequence over those slots' retained `/cmd` with delays and liveness
confirmations at each step. It is one writer but does not lock the channels —
they stay directly toggleable for troubleshooting while the sequencer is idle.

The sequence is **config-driven**: a pair of ordered step lists (`[[startup]]` /
`[[shutdown]]`) in `config.toml` define it, not Go code. Each step is one of
`cmd` (emit a retained `/cmd`), `wait_status` (wait for N slots' `/status`),
`wait_state` (wait for a slot's `/state` field, with an implicit
`/status`-online precondition so a dead device cannot pass on stale retained
state), or `delay` (a literal `duration_s` or a symbolic `network`/`stagger`
ref into `[timing]`). The subscribed topics and the `/meta` `controls`/`watches`
are derived from the configured sequence.

The default sequence (model §7.1, shipped in `config.example.toml`):
**Startup:** `power/master` on → ~30 s (network) → `power/psu-13v8` on → wait
`hf/switch` + `hf/pa-arm` + `hf/ant-switch` online → `hf/switch` trx on → wait
`hf/radio` online → `hf/switch` pa on → wait `hf/pa` power on → `hf/pa-arm`
enabled. **Shutdown** is the reverse with short staggers for inrush.

See `CLAUDE.md` for architecture and `docs/powerseq-mqtt-api.md` for the
on-the-wire contract. Shared conventions live in `../docs/`.

---

## Build / test / deploy

```bash
go build ./cmd/powerseq
go test ./...
./deploy.sh
```

Cross-compile for the Pi (shari, `192.168.1.139`):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/powerseq-linux-arm64 ./cmd/powerseq
```

---

## Configuration

Config is a 0600 TOML file (`/etc/powerseq/config.toml`) holding the
sequencer's own address (`[mqtt]`), `[timing]` (network delay, step timeout,
shutdown stagger, poll interval, default hold), `[log]`, and the config-driven
`[[startup]]` / `[[shutdown]]` step lists. See `config.example.toml` for the
full schema and the model §7.1 default sequence. The MQTT password is **not** in
the TOML — it is loaded from an `EnvironmentFile` (`/etc/powerseq/powerseq.env`)
per the
[config-and-secrets convention](../docs/conventions/config-and-secrets.md).

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).
