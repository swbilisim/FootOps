import assert from "node:assert/strict";
import { test } from "node:test";

import { check, pool, statusReason, url } from "../src/check.js";
import { settings } from "../src/logic.js";
import { FakeWeb } from "./fakes.js";

const NOW = Date.parse("2026-11-07T13:00:00Z");
const cfg = { ...settings({}), timeout: 80 };

test("the address: the home page with a cache-buster that changes every minute", () => {
  assert.equal(url(cfg, "magaza.example", NOW), `https://magaza.example/?footops-nobetci=${NOW / 60000}`);
  const c = settings({ CHECK_URL: "http://127.0.0.1:9/{domain}/?a=1" });
  assert.equal(url(c, "x.example", NOW), `http://127.0.0.1:9/x.example/?a=1&footops-nobetci=${NOW / 60000}`);
  assert.equal(settings({ CHECK_URL: "https://no-placeholder/" }).checkURL, "https://{domain}/");
});

test("answers, redirects, 404s and challenges are up; 5xx, no answer and no connection are not", async () => {
  const web = new FakeWeb();
  const cases = [
    [200, true, "200"],
    [301, true, "301"],
    [307, true, "307"],
    [404, true, "404"],
    [500, false, "500"],
    [502, false, "502"],
    [522, false, "522"],
    ["error", false, "error"],
    ["timeout", false, "timeout"],
  ];
  for (const [answer, ok, code] of cases) {
    web.site("x.example", answer);
    const r = await check("x.example", cfg, web.fetch, NOW);
    assert.equal(r.ok, ok, String(answer));
    assert.equal(r.code, code, String(answer));
  }
  web.site("x.example", "timeout");
  assert.equal((await check("x.example", cfg, web.fetch, NOW)).reason, "zaman aşımı (0 sn içinde yanıt yok)");
  web.site("x.example", new Response("challenge", { status: 403, headers: { "cf-mitigated": "challenge" } }));
  const r = await check("x.example", cfg, web.fetch, NOW);
  assert.equal(r.ok, true);
  assert.equal(r.note, "cloudflare-challenge");
});

test("visits are GETs that do not follow redirects and say who they are", async () => {
  let seen;
  await check("x.example", cfg, async (u, init) => {
    seen = init;
    return new Response("", { status: 200 });
  }, NOW);
  assert.equal(seen.method, "GET");
  assert.equal(seen.redirect, "manual");
  assert.match(seen.headers["user-agent"], /FootOps-Nobetci/);
});

test("a 530 names Cloudflare's own error code", async () => {
  const web = new FakeWeb();
  web.site("x.example", 530);
  const r = await check("x.example", cfg, web.fetch, NOW);
  assert.equal(r.reason, "530 — Cloudflare sunucuya ulaşamadı (Cloudflare 1016: alan adının köken DNS kaydı çözülemiyor)");
  assert.equal(statusReason(599), "599 — sunucu hatası");
  assert.equal(statusReason(530, "1000"), "530 — Cloudflare sunucuya ulaşamadı (Cloudflare 1000)");
});

test("a 530 whose body never ends still ends at the time limit, as a 530", async () => {
  const r = await check("x.example", cfg, async (u, init) => {
    // As fetch does: the aborted signal errors the body being read.
    const body = new ReadableStream({
      start(c) {
        init.signal.addEventListener("abort", () => c.error(new DOMException("aborted", "AbortError")));
      },
    });
    return new Response(body, { status: 530 });
  }, NOW);
  assert.equal(r.ok, false);
  assert.equal(r.code, "530");
  assert.equal(r.reason, "530 — Cloudflare sunucuya ulaşamadı");
});

test("pool runs at most n at a time, keeps the order, and each visit's clock starts with it", async () => {
  const web = new FakeWeb();
  for (let i = 0; i < 9; i++) web.site(`s${i}.example`, i < 6 ? "timeout" : 200);
  const names = Array.from({ length: 9 }, (_, i) => `s${i}.example`);
  const results = await pool(names, 6, (n) => check(n, cfg, web.fetch, NOW));
  assert.ok(web.maxOpen <= 6);
  assert.deepEqual(
    results.map((r) => r.code),
    ["timeout", "timeout", "timeout", "timeout", "timeout", "timeout", "200", "200", "200"],
  );
  assert.deepEqual(await pool([], 6, async () => 1), []);
});

test("pool starts nothing after its deadline; those items stay undefined", async () => {
  const t = Date.now();
  const out = await pool([1, 2, 3, 4], 1, async (x) => {
    await new Promise((r) => setTimeout(r, 30));
    return x;
  }, t + 45);
  assert.deepEqual(out.slice(0, 2), [1, 2]);
  assert.equal(out[3], undefined);
});

test("a visit the Worker may not make (its subrequest limit) is not a result at all", async () => {
  const r = await check("x.example", cfg, async () => {
    throw new Error("Too many subrequests.");
  }, NOW);
  assert.equal(r, null);
});
