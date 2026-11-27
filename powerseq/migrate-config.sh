#!/usr/bin/env bash
#
# Idempotent in-place migrations of an existing powerseq config.toml. deploy.sh
# runs this on the target after the seed-once step, so a config the device
# already owns picks up new required settings without a hand edit. Every
# migration checks for its own marker first; an up-to-date config is left
# byte-for-byte untouched.
#
# Usage: migrate-config.sh <config.toml>
#   exit 0 = nothing to do or migrated (prints "migrated: ..." per change)
#   exit 1 = error (file unchanged)
#
# Env: SHUTDOWN_MINUTES (default 120) — [idle] shutdown_minutes to add.
#
# Migrations:
#   1. idle shutdown (2026-10-19): a config with [[standby]] + [[shutdown]] but
#      no [[idle_shutdown]] gets `shutdown_minutes` in [idle] and an
#      [[idle_shutdown]] list = its own [[shutdown]] steps + a bwpc sleep
#      (power/shack-pc sleep, not retained). Copying the live [[shutdown]]
#      keeps any site edits to it.
#   2. auto_start chain (2026-11-27): a config with no [[auto_start]] and no
#      auto_op_slot gets auto_op_slot, auto_op_beat_s and pa_slot in [mqtt]
#      and the example [[auto_start]] list (bwpc wake, the four programs on
#      bwpc, 75auto online). The bwpc wol step copies mac, broadcast and poke
#      from the live [[startup]] wol step (pc-boot), so the real MAC stays.
set -euo pipefail

CFG="${1:?usage: migrate-config.sh <config.toml>}"
SHUTDOWN_MINUTES="${SHUTDOWN_MINUTES:-120}"
[ -f "$CFG" ] || { echo "migrate-config: $CFG not found" >&2; exit 1; }

has() { grep -Eq "$1" "$CFG"; }

# --- 1. idle shutdown -------------------------------------------------------
if has '^[[:space:]]*\[\[idle_shutdown\]\]' || has '^[[:space:]]*shutdown_minutes[[:space:]]*='; then
  : # already migrated (or hand-configured)
elif ! has '^[[:space:]]*\[\[standby\]\]' || ! has '^[[:space:]]*\[\[shutdown\]\]' || ! has '^[[:space:]]*\[idle\]'; then
  echo "migrate-config: no [[standby]]/[[shutdown]]/[idle] — idle shutdown not added"
else
  TMP="$(mktemp)"
  trap 'rm -f "$TMP"' EXIT
  awk -v mins="$SHUTDOWN_MINUTES" '
    function flush_block() {
      if (nblk > 0) { blocks = blocks "\n[[idle_shutdown]]\n" blk; nblk = 0; blk = "" }
    }
    {
      line = $0
      trimmed = line; sub(/^[ \t]+/, "", trimmed)
      is_header = (substr(trimmed, 1, 1) == "[")

      # [idle]: add shutdown_minutes right after the header (keys of a table
      # must precede the next header; order within the table is free).
      if (trimmed ~ /^\[idle\][ \t]*(#.*)?$/) {
        print line
        print "# Full power-down after this long without activity (> timeout_minutes)."
        print "# Added by migrate-config.sh; 0 disables (remove [[idle_shutdown]] too)."
        print "shutdown_minutes = " mins
        in_shut = 0
        next
      }

      # Collect the key lines of every [[shutdown]] block (comments and blank
      # lines dropped; a block ends at the next table header).
      if (is_header) {
        if (in_shut) flush_block()
        in_shut = (trimmed ~ /^\[\[shutdown\]\][ \t]*(#.*)?$/)
        print line
        next
      }
      if (in_shut && trimmed != "" && substr(trimmed, 1, 1) != "#") {
        blk = blk line "\n"; nblk++
        if (trimmed ~ /^name[ \t]*=[ \t]*"pc-sleep"/) has_sleep = 1
      }
      print line
    }
    END {
      if (in_shut) flush_block()
      print ""
      print "# --- idle shutdown (added by migrate-config.sh) ------------------------------"
      print "# The [[shutdown]] steps plus a shack PC sleep, run from standby once"
      print "# [idle] shutdown_minutes have passed since the last activity."
      printf "%s", blocks
      if (has_sleep) exit
      print ""
      print "[[idle_shutdown]]"
      print "name = \"pc-sleep\""
      print "kind = \"cmd\""
      print "slot = \"power/shack-pc\""
      print "action = \"sleep\""
      print "value = \"now\""
      print "retain = false"
    }
  ' "$CFG" > "$TMP"
  cat "$TMP" > "$CFG"   # in place: keeps owner + mode of the 0600 file
  echo "migrated: idle shutdown (shutdown_minutes = ${SHUTDOWN_MINUTES}, [[idle_shutdown]] = [[shutdown]] + pc-sleep)"
fi

# --- 2. auto_start chain ----------------------------------------------------
if has '^[[:space:]]*\[\[auto_start\]\]' || has '^[[:space:]]*auto_op_slot[[:space:]]*='; then
  : # already migrated (or hand-configured)
elif ! has '^[[:space:]]*\[mqtt\]'; then
  echo "migrate-config: no [mqtt] — auto_start chain not added"
else
  TMP2="$(mktemp)"
  trap 'rm -f "${TMP:-}" "$TMP2"' EXIT
  awk '
    {
      line = $0
      trimmed = line; sub(/^[ \t]+/, "", trimmed)
      if (trimmed ~ /^\[mqtt\][ \t]*(#.*)?$/) {
        print line
        print "# 75auto session hold, auto_start refusal and PA standby on the session"
        print "# start. Added by migrate-config.sh; \"\" disables."
        print "auto_op_slot = \"auto-op\""
        print "auto_op_beat_s = 30"
        print "pa_slot = \"pa\""
        next
      }
      # The first wol step of [[startup]] (pc-boot): keep its mac/broadcast/poke.
      if (substr(trimmed, 1, 1) == "[") {
        if (in_start && is_wol) done_wol = 1
        in_start = (trimmed ~ /^\[\[startup\]\][ \t]*(#.*)?$/) && !done_wol
        is_wol = 0; m = ""; b = ""; p = ""
      } else if (in_start) {
        if (trimmed ~ /^kind[ \t]*=[ \t]*"wol"/) is_wol = 1
        if (trimmed ~ /^mac[ \t]*=/) m = line
        if (trimmed ~ /^broadcast[ \t]*=/) b = line
        if (trimmed ~ /^poke[ \t]*=/) p = line
        if (is_wol) { wm = m; wb = b; wp = p }
      }
      print line
    }
    function step(name, kind) {
      print ""
      print "[[auto_start]]"
      print "name = \"" name "\""
      print "kind = \"" kind "\""
    }
    END {
      if (wm == "") wm = "mac = \"00:00:00:00:00:00\"    # REPLACE with the bwpc NIC MAC"
      print ""
      print "# --- auto_start chain (added by migrate-config.sh) ---------------------------"
      print "# The 75AUTO Start of hf_console: startup or wake first, then these steps."
      print "# The step name is the progress row of the page. See config.example.toml."
      step("bwpc", "wol")
      print wm
      if (wb != "") print wb
      if (wp != "") print wp
      step("bwpc", "wait_status")
      print "slots = [\"power/shack-pc\"]"
      print "timeout_s = 300"
      n = split("aethersdr wsjtx 75auto", progs, " ")
      for (i = 1; i <= n; i++) {
        step(progs[i], "cmd")
        print "slot = \"power/shack-pc\""
        print "action = \"ensure_program\""
        print "value = \"" progs[i] "\""
        print "retain = false"
        step(progs[i], "wait_state")
        print "slot = \"power/shack-pc\""
        print "field = \"program_" progs[i] "\""
        print "value = \"ready\""
        print "fresh = true"
        print "fail_value = \"failed\""
      }
      step("75auto", "wait_status")
      print "slots = [\"hf/auto-op\"]"
    }
  ' "$CFG" > "$TMP2"
  cat "$TMP2" > "$CFG"   # in place: keeps owner + mode of the 0600 file
  echo "migrated: auto_start chain ([mqtt] auto_op_slot/auto_op_beat_s/pa_slot + [[auto_start]])"
fi
