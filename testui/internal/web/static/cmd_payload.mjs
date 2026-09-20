// cmd_payload.mjs — the /cmd payload builder, extracted so `node --test` can
// pin the wire contract (pure functions; no DOM).
//
// The value's wire type rides ONLY on the command descriptor's value_type —
// never on the field's render type. Station convention: /cmd args are strings
// under the value key; a raw JSON boolean is misparsed by the m5stamp firmware
// (ArduinoJson cannot coerce bool→string, so `set_enabled true` arrives as ""
// and the arm drops — the 2026-09 review finding T3). `bool` (atr1k
// set_inline) is the one receiver that contractually requires a real JSON
// boolean. int/float stay numbers (the Go receivers unmarshal numbers).
// See docs/cmd-convention-audit.md.

// Coerce a raw input value to the wire type the command descriptor declares.
export function cmdValue(cmd, val) {
  switch (cmd.value_type) {
    case 'bool':    return val === true || val === 'true';
    case 'boolean': return String(val === true || val === 'true');
    default:        return val; // rows coerce numbers; strings pass through
  }
}

// Build the /cmd JSON payload from an expose command descriptor.
//   action != ""  -> {"action":<action>, <value_key>:<value>}
//   action == ""  -> {<value_key>:<value>}        (value-key-only, e.g. {"select":"port2"})
//   no value_key  -> {"action":<action>}          (button)
export function buildPayload(cmd, value) {
  const hasAction = cmd.action && cmd.action !== '';
  const hasKey = cmd.value_key && cmd.value_key !== '';
  if (hasAction && hasKey) return { action: cmd.action, [cmd.value_key]: cmdValue(cmd, value) };
  if (!hasAction && hasKey) return { [cmd.value_key]: cmdValue(cmd, value) };
  if (hasAction && !hasKey) return { action: cmd.action };
  return {};
}
