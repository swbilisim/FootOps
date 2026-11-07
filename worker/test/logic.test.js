import assert from "node:assert/strict";
import { test } from "node:test";

import { HOUR, MIN, SEND_MAX, clean, localTime, pack, parseState, serverCodes, settings, slot, spanTR, visitBudget } from "../src/logic.js";

test("settings: defaults, and values out of bounds fall back to them", () => {
  const d = settings({});
  assert.equal(d.every, 2);
  assert.equal(d.failAfter, 3);
  assert.equal(d.recoverAfter, 2);
  assert.equal(d.silentAfter, 15 * MIN);
  assert.equal(d.backupAfter, 26 * HOUR);
  assert.equal(d.timeout, 10_000);
  assert.equal(d.maxChecks, 40);
  assert.equal(d.dryRun, false);
  assert.equal(d.telegramAPI, "https://api.telegram.org");
  const c = settings({ CHECK_EVERY_MIN: "5", FAIL_AFTER: "0", SILENT_AFTER_MIN: "abc", DRY_RUN: "evet", TELEGRAM_API: "http://x:1/" });
  assert.equal(c.every, 5);
  assert.equal(c.failAfter, 3);
  assert.equal(c.silentAfter, 15 * MIN);
  assert.equal(c.dryRun, true);
  assert.equal(c.telegramAPI, "http://x:1");
  assert.equal(settings({ CHECK_EVERY_MIN: "61" }).every, 2);
  // A visit may not outlast what keeps runs apart.
  assert.equal(settings({ TIMEOUT_SEC: "30" }).timeout, 10_000);
  assert.equal(settings({ TIMEOUT_SEC: "15" }).timeout, 15_000);
});

test("serverCodes: R2 bindings named SERVER_<code>, in order", () => {
  const b = { get() {} };
  assert.deepEqual(serverCodes({ SERVER_H1: b, SERVER_A: b, STATE: b, SERVER_x: b, SERVER_TEXT: "not a bucket" }), ["A", "H1"]);
});

test("spanTR is FootOps' way of writing durations", () => {
  assert.equal(spanTR(0), "1 dk");
  assert.equal(spanTR(59 * MIN), "59 dk");
  assert.equal(spanTR(60 * MIN), "1 sa");
  assert.equal(spanTR(125 * MIN), "2 sa 5 dk");
  assert.equal(spanTR(48 * HOUR), "2 gün");
  assert.equal(spanTR(27 * HOUR), "1 gün 3 sa");
});

test("localTime is day.month hour:minute in the zone given", () => {
  const t = Date.parse("2026-11-07T21:05:00Z");
  assert.equal(localTime(t, "Europe/Istanbul"), "08.11 00:05");
  assert.equal(localTime(t, "UTC"), "07.11 21:05");
  assert.equal(localTime(t, "No/Such_Zone"), "2026-11-07 21:05 UTC");
});

test("slot spreads domains over the minutes", () => {
  const n = Array.from({ length: 500 }, (_, i) => `site${i}.example`);
  const count = [0, 0, 0, 0, 0];
  for (const d of n) count[slot(d, 5)]++;
  for (const c of count) assert.ok(c > 70 && c < 130, String(count));
  assert.equal(slot("x.example", 1), 0);
});

test("parseState: garbage and other formats start afresh", () => {
  for (const text of [null, "", "{", "[]", '{"format":2}', '"x"']) {
    const st = parseState(text);
    assert.deepEqual(st, { format: 1, servers: {}, domains: {} }, String(text));
  }
  const st = parseState('{"format":1,"servers":{"A":{"name":"A"},"B":3},"domains":{"x.example":{"fails":1},"y":null}}');
  assert.deepEqual(Object.keys(st.servers), ["A"]);
  assert.deepEqual(Object.keys(st.domains), ["x.example"]);
});

test("pack: blocks join into messages within Telegram's length", () => {
  const blocks = [
    { text: "a".repeat(30), evs: [1] },
    { text: "b".repeat(30), evs: [2] },
    { text: "c".repeat(30), evs: [3] },
  ];
  const out = pack(blocks, 70);
  assert.equal(out.length, 2);
  assert.equal(out[0].text, "a".repeat(30) + "\n\n" + "b".repeat(30));
  assert.deepEqual(out[0].evs, [1, 2]);
  assert.deepEqual(out[1].evs, [3]);
  assert.equal(pack([{ text: "x".repeat(100), evs: [1] }], 50)[0].text.length, 50);
});

test("visitBudget: what the subrequests leave after R2 and the messages", () => {
  const c = settings({});
  assert.equal(SEND_MAX, 3);
  assert.equal(visitBudget(c, 2), 40); // 50 - 6 - 3 = 41, MAX_CHECKS 40
  assert.equal(visitBudget(c, 4), 37);
  assert.equal(visitBudget(c, 30), 0);
  assert.equal(visitBudget(settings({ SUBREQUESTS: "1000", MAX_CHECKS: "200" }), 4), 200);
});

test("clean: a name keeps letters, digits, spaces and ._- only", () => {
  assert.equal(clean("magaza-1.örnek_x", 64), "magaza-1.örnek_x");
  assert.equal(clean("a\nb\u202ec<d>", 64), "a?b?c?d?");
  assert.equal(clean("x".repeat(80), 10), "xxxxxxxxx…");
});

test("localTime: no time is a question mark, not an error", () => {
  assert.equal(localTime(NaN, "Europe/Istanbul"), "?");
  assert.equal(localTime(Date.parse("nonsense"), "UTC"), "?");
});
