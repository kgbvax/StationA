// Wire-contract tests for the /cmd payload builder (run: node --test
// internal/web/static/). Pins review finding T3: a JSON-boolean value under
// the value key is misparsed by the m5stamp firmware (ArduinoJson cannot
// coerce bool→string → set_enabled true reads as "" → the arm drops), while
// atr1k set_inline contractually REQUIRES a real JSON boolean.
import test from 'node:test';
import assert from 'node:assert/strict';
import { buildPayload, cmdValue } from './cmd_payload.mjs';

test('set_enabled ships the value as a string, never a JSON boolean (T3)', () => {
  const cmd = { action: 'set_enabled', value_key: 'value', value_type: 'boolean' };
  assert.deepEqual(buildPayload(cmd, 'true'), { action: 'set_enabled', value: 'true' });
  assert.deepEqual(buildPayload(cmd, 'false'), { action: 'set_enabled', value: 'false' });
  assert.equal(cmdValue(cmd, 'true'), 'true');
});

test('atr1k set_inline keeps the real JSON boolean its parser requires', () => {
  const cmd = { action: 'set_inline', value_key: 'value', value_type: 'bool' };
  assert.deepEqual(buildPayload(cmd, 'true'), { action: 'set_inline', value: true });
  assert.deepEqual(buildPayload(cmd, 'false'), { action: 'set_inline', value: false });
});

test('numbers coerce through the row path and pass buildPayload unchanged', () => {
  const cmd = { action: 'set_freq', value_key: 'freq_hz', value_type: 'int' };
  assert.deepEqual(buildPayload(cmd, 432650000), { action: 'set_freq', freq_hz: 432650000 });
});

test('value-key-only and action-only shapes unchanged', () => {
  assert.deepEqual(buildPayload({ value_key: 'select' }, 'port2'), { select: 'port2' });
  assert.deepEqual(buildPayload({ action: 'stop' }, null), { action: 'stop' });
  assert.deepEqual(buildPayload({}, null), {});
});
