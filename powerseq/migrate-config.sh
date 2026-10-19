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
