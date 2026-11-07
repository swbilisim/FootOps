import assert from "node:assert/strict";
import { test } from "node:test";

import { run } from "../src/index.js";
import { BEAT_KEY, MIN, STATE_KEY, STATUS_KEY } from "../src/logic.js";
import { FakeBucket, FakeWeb, slow, status } from "./fakes.js";

const T0 = Date.parse("2026-11-07T13:00:00Z");
const TOKEN = "123456789:AAsecretpartofthetoken";
const MAGAZA = ["magaza.example", "www.magaza.example"];

// world builds two servers: A1 with two sites (three domains), B2 with one.
function world(vars = {}) {
  const env = {
    SERVER_A1: new FakeBucket(),
    SERVER_B2: new FakeBucket(),
    STATE: new FakeBucket(),
    TELEGRAM_TOKEN: TOKEN,
    TELEGRAM_CHAT: "-1001",
    CHECK_EVERY_MIN: "1",
    ...vars,
  };
  const web = new FakeWeb();
  const logs = [];
  const at = (t, extra = {}) => run(env, { now: t, fetch: web.fetch, log: (l) => logs.push(l), ...extra });
  // Status files as R2 would hold them: uploaded when the server wrote them.
  const a1 = (t, a = {}) =>
    env.SERVER_A1.set(STATUS_KEY, status("A1", a.time ?? t, a.sites ?? [["magaza", MAGAZA], ["blog", ["blog.example"]]], a.backup), t);
  const b2 = (t, b = {}) => env.SERVER_B2.set(STATUS_KEY, status("B2", b.time ?? t, b.sites ?? [["kasa", ["kasa.example"]]], b.backup), t);
  const write = (t, a = {}, b = {}) => {
    a1(t, a);
    b2(t, b);
  };
  return { env, web, logs, at, write, a1, b2 };
}

const texts = (web) => web.sent.map((m) => m.text);

test("first run greets the chat, visits every domain, writes the state and both heartbeats", async () => {
  const w = world();
  w.write(T0);
  const out = await w.at(T0);
  assert.equal(out.visit.length, 4);
  assert.equal(w.web.sent.length, 1);
  const msg = w.web.sent[0];
  assert.equal(msg.chat_id, "-1001");
  assert.equal(msg.token, TOKEN);
  assert.match(msg.text, /^🟢 FootOps nöbetçisi çalışıyor\./);
  assert.match(msg.text, /A1: 2 site, 3 alan adı · B2: 1 site, 1 alan adı/);
  assert.match(msg.text, /dakikada bir/);
  assert.deepEqual(msg.link_preview_options, { is_disabled: true });
  assert.ok(w.env.STATE.json(STATE_KEY).welcomed);
  const beat = w.env.SERVER_A1.json(BEAT_KEY);
  assert.equal(beat.format, 1);
  assert.equal(beat.time, new Date(T0).toISOString());
  assert.equal(beat.send_error, undefined);
  assert.deepEqual(
    beat.domains.map((d) => [d.name, d.ok]),
    [
      ["blog.example", true],
      ["magaza.example", true],
      ["www.magaza.example", true],
    ],
  );
  assert.equal(w.env.SERVER_B2.json(BEAT_KEY).domains.length, 1);
  // Nothing new: the second run says nothing and does not write the state.
  const puts = w.env.STATE.puts;
  await w.at(T0 + MIN);
  assert.equal(w.web.sent.length, 1);
  assert.equal(w.env.STATE.puts, puts);
  for (const l of w.logs) assert.ok(!l.includes("AAsecret"), "token in the log");
});

test("a site down three visits in a row is one message, and so is its recovery", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  w.web.site("magaza.example", 502);
  w.web.site("www.magaza.example", 502);
  await w.at(T0 + MIN);
  await w.at(T0 + 2 * MIN);
  assert.equal(w.web.sent.length, 1, "nothing before the third failure");
  await w.at(T0 + 3 * MIN);
  assert.equal(w.web.sent.length, 2);
  const down = w.web.sent[1].text;
  assert.match(down, /^❌ \[A1\] magaza \(2 alan adı\) çalışmıyor: 502 — site yanıt vermiyor/);
  assert.match(down, /magaza\.example, www\.magaza\.example/);
  // The heartbeat changed at once, not five minutes later.
  const beat = w.env.SERVER_A1.json(BEAT_KEY);
  assert.equal(beat.time, new Date(T0 + 3 * MIN).toISOString());
  const m = beat.domains.find((d) => d.name === "magaza.example");
  assert.equal(m.ok, false);
  assert.equal(m.down, true);
  assert.equal(m.code, "502");
  assert.equal(m.since, new Date(T0 + MIN).toISOString());

  // Down stays down quietly; one answer is not enough, two are.
  await w.at(T0 + 4 * MIN);
  w.web.site("magaza.example", 200);
  w.web.site("www.magaza.example", 301);
  await w.at(T0 + 5 * MIN);
  assert.equal(w.web.sent.length, 2);
  await w.at(T0 + 6 * MIN);
  assert.equal(w.web.sent.length, 3);
  assert.equal(w.web.sent[2].text, "✅ [A1] magaza (2 alan adı) yeniden açılıyor (kesinti 4 dk)\nmagaza.example, www.magaza.example");
  await w.at(T0 + 7 * MIN);
  assert.equal(w.web.sent.length, 3);
});

test("a blip is forgotten; a failure between two answers of a recovery restarts it", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  w.web.site("blog.example", "error");
  await w.at(T0 + MIN);
  await w.at(T0 + 2 * MIN);
  w.web.site("blog.example", 200);
  await w.at(T0 + 3 * MIN);
  w.web.site("blog.example", "error");
  await w.at(T0 + 4 * MIN);
  await w.at(T0 + 5 * MIN);
  assert.equal(w.web.sent.length, 1, "two failures, an answer, two failures: no message");
  await w.at(T0 + 6 * MIN);
  assert.match(w.web.sent[1].text, /^❌ \[A1\] blog\.example çalışmıyor: bağlantı kurulamadı \(Network connection lost\.\)/);
  w.web.site("blog.example", 200);
  await w.at(T0 + 7 * MIN);
  w.web.site("blog.example", "error");
  await w.at(T0 + 8 * MIN);
  w.web.site("blog.example", 200);
  await w.at(T0 + 9 * MIN);
  assert.equal(w.web.sent.length, 2);
  await w.at(T0 + 10 * MIN);
  // The outage is the second run of failures (from +4) to the first answer that held (+9).
  assert.equal(w.web.sent[2].text, "✅ [A1] blog.example yeniden açılıyor (kesinti 5 dk)");
});

test("back, but the chat not told before it fails again: the news keeps both outages' times", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  w.web.site("blog.example", 502);
  for (let i = 1; i <= 3; i++) await w.at(T0 + i * MIN);
  assert.equal(w.web.sent.length, 2);
  w.web.site("blog.example", 200);
  w.web.telegram = 500;
  await w.at(T0 + 4 * MIN);
  await w.at(T0 + 5 * MIN); // back for the watcher; the message does not go
  w.web.site("blog.example", 502);
  await w.at(T0 + 6 * MIN);
  w.web.telegram = 200;
  await w.at(T0 + 7 * MIN); // "back" goes now, with its time; the new run of failures goes on
  assert.equal(w.web.sent[2].text, "✅ [A1] blog.example yeniden açılıyor (kesinti 3 dk)");
  await w.at(T0 + 8 * MIN);
  assert.match(w.web.sent[3].text, /^❌ \[A1\] blog\.example çalışmıyor/);
  const beat = w.env.SERVER_A1.json(BEAT_KEY).domains.find((d) => d.name === "blog.example");
  assert.equal(beat.since, new Date(T0 + 6 * MIN).toISOString(), "the new outage's start");
});

test("a whole server going down is one message, held until all its domains decided", async () => {
  const w = world({ CHECK_EVERY_MIN: "5" });
  w.write(T0);
  await w.at(T0);
  for (const d of [...MAGAZA, "blog.example"]) w.web.site(d, 522);
  // Visits come by turns; once one fails, every domain of A1 is visited each minute.
  for (let i = 1; i <= 8; i++) await w.at(T0 + i * MIN);
  const downs = texts(w.web).filter((t) => t.startsWith("❌"));
  assert.equal(downs.length, 1, texts(w.web).join("\n---\n"));
  assert.match(downs[0], /^❌ \[A1\] Sitelerin hepsi \(2 site, 3 alan adı\) çalışmıyor: 522 — sunucuya bağlanılamadı \(kapalı ya da ağı yok\)$/);
});

test("a server whose visits hang does not crowd out the others; its own domains take turns", async () => {
  const w = world();
  const many = Array.from({ length: 22 }, (_, i) => `a${String(i).padStart(2, "0")}.example`);
  w.write(T0, { sites: [["cok", many]] });
  for (const d of many) w.web.site(d, slow(40));
  const seenA = new Set();
  for (let i = 0; i < 4; i++) {
    w.web.visits = [];
    const out = await w.at(T0 + i * MIN, { visitsFor: 100 });
    assert.ok(w.web.visits.includes("kasa.example"), `run ${i}: the other server not visited`);
    assert.ok(out.results.filter(Boolean).length < many.length + 1, "the time limit did not bite");
    w.web.visits.forEach((d) => seenA.add(d));
  }
  assert.equal([...seenA].filter((d) => d.startsWith("a")).length, 22, "some domains never got a turn");
});

test("every few minutes: a working domain is visited once per interval, spread over the minutes", async () => {
  const w = world({ CHECK_EVERY_MIN: "5" });
  w.write(T0);
  for (let i = 0; i < 10; i++) await w.at(T0 + i * MIN);
  const count = {};
  for (const v of w.web.visits) count[v] = (count[v] || 0) + 1;
  assert.deepEqual(Object.values(count), [2, 2, 2, 2], JSON.stringify(count));
});

test("a server that stops writing is news after the silence, with what its sites do", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  // A1's file stops changing; its sites still answer.
  for (let i = 1; i <= 15; i++) {
    w.b2(T0 + i * MIN);
    await w.at(T0 + i * MIN);
  }
  assert.equal(w.web.sent.length, 1, "15 minutes is not yet silence");
  await w.at(T0 + 16 * MIN);
  assert.equal(w.web.sent.length, 2);
  assert.equal(
    w.web.sent[1].text,
    "🔴 [A1] FootOps'tan 16 dk haber yok (son haber 07.11 16:00)\nSiteleri açılıyor: sunucu çalışıyor; FootOps'un agent'ı durmuş ya da yedek kovasına yazamıyor olabilir.",
  );
  await w.at(T0 + 17 * MIN);
  assert.equal(w.web.sent.length, 2);
  w.write(T0 + 21 * MIN);
  await w.at(T0 + 22 * MIN);
  assert.equal(w.web.sent[2].text, "✅ [A1] FootOps'tan yeniden haber var (sessizlik 21 dk sürdü)");
});

test("silence is judged by R2's clock, not by the server's", async () => {
  const w = world();
  // A1's clock is 40 minutes behind; its file reached R2 just now.
  w.write(T0, { time: T0 - 40 * MIN });
  await w.at(T0);
  assert.equal(texts(w.web).filter((t) => t.includes("haber yok")).length, 0);
});

test("a silent server whose sites are down too: the second line says so", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  for (const d of [...MAGAZA, "blog.example"]) w.web.site(d, 522);
  for (let i = 1; i <= 16; i++) {
    w.b2(T0 + i * MIN);
    await w.at(T0 + i * MIN);
  }
  const t = texts(w.web);
  assert.equal(t.length, 3, t.join("\n---\n"));
  assert.match(t[1], /^❌ \[A1\] Sitelerin hepsi \(2 site, 3 alan adı\) çalışmıyor: 522/);
  assert.match(t[2], /^🔴 \[A1\] FootOps'tan 16 dk haber yok \(son haber 07\.11 16:00\)\nSitelerine de ulaşılamıyor: sunucu kapalı ya da ağı gitmiş olabilir\.$/);
  // Back: the sites and the news, each once.
  for (const d of [...MAGAZA, "blog.example"]) w.web.site(d, 200);
  w.write(T0 + 17 * MIN);
  await w.at(T0 + 17 * MIN);
  await w.at(T0 + 18 * MIN);
  const back = texts(w.web).slice(3).join("\n---\n");
  assert.match(back, /✅ \[A1\] FootOps'tan yeniden haber var \(sessizlik 17 dk sürdü\)/);
  assert.match(back, /✅ \[A1\] Sitelerin hepsi \(2 site, 3 alan adı\) yeniden açılıyor \(kesinti 16 dk\)/);
  assert.equal(texts(w.web).length, 5, back);
});

test("a missing or unreadable status file", async () => {
  const w = world();
  w.b2(T0);
  await w.at(T0);
  assert.match(w.web.sent[0].text, /🔴 \[A1\] FootOps'tan haber yok\nDurum dosyası yok\./);
  // A file that cannot be read for a while keeps the last news and the domains.
  w.write(T0 + MIN);
  await w.at(T0 + MIN);
  assert.match(w.web.sent[1].text, /^✅ \[A1\] FootOps'tan yeniden haber var/);
  w.env.SERVER_A1.failGet = "R2 hatası";
  const out = await w.at(T0 + 2 * MIN);
  assert.ok(out.visit.includes("blog.example"));
  assert.match(w.logs.at(-1), /A1 durum dosyası okunamadı: R2 hatası/);
  assert.equal(w.web.sent.length, 2);
  await w.at(T0 + 17 * MIN);
  assert.match(w.web.sent[2].text, /^🔴 \[A1\] FootOps'tan 16 dk haber yok .*\nDurum dosyası okunamadı: R2 hatası/);
});

test("a file that says its site list could not be read keeps the domains", async () => {
  const w = world();
  w.write(T0);
  w.web.site("blog.example", 502);
  for (let i = 0; i < 3; i++) await w.at(T0 + i * MIN);
  const f = status("A1", T0 + 3 * MIN, []);
  f.sites_error = "footops.json okunamadı";
  w.env.SERVER_A1.set(STATUS_KEY, f, T0 + 3 * MIN);
  const out = await w.at(T0 + 3 * MIN);
  assert.ok(out.visit.includes("blog.example"));
  assert.equal(texts(w.web).filter((t) => t.includes("izlenmiyor")).length, 0);
  assert.match(w.logs.at(-1), /A1 .*\(site listesi: footops\.json okunamadı\)/);
});

test("backups older than a day and two hours, then back, or paused", async () => {
  const w = world();
  const old = T0 - 27 * 60 * MIN;
  w.write(T0, { backup: { ready: true, last_ok: old } });
  await w.at(T0);
  assert.match(w.web.sent[0].text, /🟡 \[A1\] Son başarılı yedek 1 gün 3 sa önce \(06\.11 13:00\): yedekler alınmıyor olabilir/);
  w.write(T0 + MIN, { backup: { ready: true, last_ok: T0 + MIN } });
  await w.at(T0 + MIN);
  assert.equal(w.web.sent[1].text, "✅ [A1] Yedekler yeniden alınıyor (son yedek 07.11 16:01)");
  w.write(T0 + 2 * MIN, { backup: { ready: true, last_ok: old } });
  await w.at(T0 + 2 * MIN);
  w.write(T0 + 3 * MIN, { backup: { ready: true, paused: true, last_ok: old } });
  await w.at(T0 + 3 * MIN);
  assert.equal(w.web.sent.at(-1).text, "✅ [A1] Yedek gecikmesi uyarısı kalktı: yedekleme duraklatıldı.");
});

test("Go's zero time is no backup at all, and a late warning clears without a time", async () => {
  const w = world();
  w.write(T0, { backup: { ready: true, last_ok: "0001-01-01T00:00:00Z" } });
  await w.at(T0);
  assert.equal(texts(w.web).filter((t) => t.includes("Son başarılı yedek")).length, 0, texts(w.web).join("\n"));
  w.write(T0 + MIN, { backup: { ready: true, last_ok: T0 - 30 * 60 * MIN } });
  await w.at(T0 + MIN);
  assert.match(w.web.sent.at(-1).text, /^🟡 \[A1\]/);
  // A new repository: nothing saved yet (Go leaves last_ok out).
  w.write(T0 + 2 * MIN, { backup: { ready: true } });
  await w.at(T0 + 2 * MIN);
  assert.equal(w.web.sent.at(-1).text, "✅ [A1] Yedek gecikmesi uyarısı kalktı.");
});

test("a message Telegram refuses is sent again the next run, and the heartbeat says so", async () => {
  const w = world();
  w.write(T0);
  w.web.telegram = 403;
  await w.at(T0);
  assert.equal(w.web.sent.length, 0);
  assert.match(w.logs[0], /0\/1 mesaj .*mesaj gönderilemedi: Telegram reddetti \(HTTP 403: Forbidden: bot was blocked by the user\)/);
  assert.equal(w.env.STATE.json(STATE_KEY).welcomed, undefined);
  const beat = w.env.SERVER_A1.json(BEAT_KEY);
  assert.match(beat.send_error, /HTTP 403/);
  assert.equal(beat.send_failing, new Date(T0).toISOString());
  w.web.telegram = 200;
  await w.at(T0 + MIN);
  assert.equal(w.web.sent.length, 1);
  assert.match(w.web.sent[0].text, /^🟢/);
  await w.at(T0 + 6 * MIN);
  const after = w.env.SERVER_A1.json(BEAT_KEY);
  assert.equal(after.send_error, undefined);
  assert.equal(after.send_failing, undefined);
});

test("a chat that became a supergroup is followed to its new id", async () => {
  const w = world();
  w.write(T0);
  w.web.migrate = { from: "-1001", to: "-100777" };
  await w.at(T0);
  assert.equal(w.web.sent.length, 1);
  assert.equal(w.web.sent[0].chat_id, "-100777");
  w.web.site("blog.example", 502);
  for (let i = 1; i <= 3; i++) await w.at(T0 + i * MIN);
  assert.equal(w.web.sent[1].chat_id, "-100777");
  assert.equal(w.web.calls, 3, "the old id was asked once");
});

test("at most three messages a run; the rest wait", async () => {
  const w = world();
  const many = Array.from({ length: 30 }, (_, i) => `s${String(i).padStart(2, "0")}.example`);
  w.write(T0, { sites: many.map((d, i) => [`s${i}`, [d]]) }, { sites: [] });
  await w.at(T0);
  const calls = w.web.calls;
  // Each down for a cause of its own: one block each, too many for one message.
  many.forEach((d, i) => w.web.site(d, () => new Response("y".repeat(10), { status: 500 + i })));
  for (let i = 1; i <= 3; i++) await w.at(T0 + i * MIN);
  assert.ok(w.web.calls - calls <= 3, `${w.web.calls - calls} messages in one run`);
});

test("without the Telegram secrets nothing is marked told", async () => {
  const w = world({ TELEGRAM_TOKEN: "" });
  w.write(T0);
  await w.at(T0);
  assert.match(w.logs[0], /TELEGRAM_TOKEN \/ TELEGRAM_CHAT tanımlı değil/);
  assert.equal(w.env.STATE.json(STATE_KEY)?.welcomed, undefined);
});

test("a dry run logs the messages; leaving it starts afresh", async () => {
  const w = world({ DRY_RUN: "1" });
  w.write(T0);
  w.web.site("blog.example", 502);
  for (let i = 0; i < 3; i++) await w.at(T0 + i * MIN);
  assert.equal(w.web.sent.length, 0);
  const all = w.logs.join("\n");
  assert.match(all, /nöbetçi \(kuru\)/);
  assert.match(all, /\[kuru çalıştırma, gönderilmedi\] 🟢 FootOps nöbetçisi çalışıyor\./);
  assert.match(all, /\[kuru çalıştırma, gönderilmedi\] ❌ \[A1\] blog\.example çalışmıyor: 502/);
  assert.match(all, /✓ A1 magaza\.example 200 \d+ ms/);
  w.env.DRY_RUN = "";
  await w.at(T0 + 3 * MIN);
  assert.equal(w.web.sent.length, 1);
  assert.match(w.web.sent[0].text, /^🟢/);
  assert.equal(w.env.STATE.json(STATE_KEY).dry, undefined);
});

test("a domain gone from FootOps' list after it was reported down, and a server taken off", async () => {
  const w = world();
  w.write(T0);
  w.web.site("blog.example", 503);
  for (let i = 0; i < 3; i++) await w.at(T0 + i * MIN);
  assert.equal(w.web.sent.length, 2);
  w.write(T0 + 3 * MIN, { sites: [["magaza", MAGAZA]] });
  await w.at(T0 + 3 * MIN);
  assert.equal(w.web.sent[2].text, "⚪ [A1] blog.example artık izlenmiyor (FootOps'un site listesinde yok); çalışmıyor diye bildirilmişti.");
  assert.equal(w.env.STATE.json(STATE_KEY).domains["blog.example"], undefined);
  // A server off the list is forgotten quietly, unless the chat knows it silent.
  delete w.env.SERVER_B2;
  await w.at(T0 + 4 * MIN);
  assert.equal(w.web.sent.length, 3);
  assert.equal(w.env.STATE.json(STATE_KEY).servers.B2, undefined);
});

test("a parked site, invalid domains and IP addresses are not visited; a domain on two servers once", async () => {
  const w = world();
  w.write(
    T0,
    {
      sites: [
        ["magaza", ["magaza.example", "Bad Domain", "evil.example/path", "x", "10.0.0.1", "1.2.3.4"]],
        ["park", ["park.example"], "parked"],
      ],
    },
    { sites: [["kasa", ["kasa.example", "magaza.example"]]] },
  );
  const out = await w.at(T0);
  assert.deepEqual([...out.visit].sort(), ["kasa.example", "magaza.example"]);
});

test("names from a status file cannot pass for a line of their own", async () => {
  const w = world();
  w.write(T0, { sites: [["blog\n✅ [B2] sahte", ["blog.example"], "stopped\nx"]] });
  w.web.site("blog.example", 502);
  for (let i = 0; i < 3; i++) await w.at(T0 + i * MIN);
  const msg = w.web.sent[1].text;
  assert.equal(msg.split("\n").length, 1, msg);
  assert.equal(w.env.STATE.json(STATE_KEY).domains["blog.example"].site, "blog?? ?B2? sahte");
});

test("a stopped container is named in the message", async () => {
  const w = world();
  w.write(T0, { sites: [["blog", ["blog.example"], "stopped"]] });
  w.web.site("blog.example", 502);
  for (let i = 0; i < 3; i++) await w.at(T0 + i * MIN);
  assert.equal(w.web.sent[1].text, "❌ [A1] blog.example çalışmıyor: 502 — site yanıt vermiyor · container durmuş");
});

test("a domain Cloudflare answers with a challenge cannot be watched: said once, and when it ends", async () => {
  const w = world();
  w.write(T0);
  w.web.site("blog.example", () => new Response("", { status: 403, headers: { "cf-mitigated": "challenge" } }));
  await w.at(T0);
  assert.match(w.web.sent[0].text, /⚠️ \[A1\] blog\.example izlenemiyor: Cloudflare ziyareti bir doğrulama sayfasıyla karşılıyor/);
  assert.equal(w.env.SERVER_A1.json(BEAT_KEY).domains.find((d) => d.name === "blog.example").note, "cloudflare-challenge");
  await w.at(T0 + MIN);
  assert.equal(w.web.sent.length, 1);
  w.web.site("blog.example", 200);
  await w.at(T0 + 2 * MIN);
  assert.equal(w.web.sent[1].text, "✅ [A1] blog.example yeniden izlenebiliyor.");
});

test("a state written by another run in between is not overwritten", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  const orig = w.env.STATE.get.bind(w.env.STATE);
  w.env.STATE.get = async (key) => {
    const o = await orig(key);
    w.env.STATE.set(STATE_KEY, { format: 1, servers: {}, domains: {}, welcomed: "other" });
    return o;
  };
  w.web.site("blog.example", 500);
  const out = await w.at(T0 + MIN);
  assert.match(w.logs.at(-1), /durum kaydedilmedi \(başka çalıştırma araya girdi\)/);
  assert.equal(w.env.STATE.json(STATE_KEY).welcomed, "other");
  assert.ok(out.state.domains["blog.example"].fails === 1);
});

test("a state that cannot be written is logged, and the heartbeat says so", async () => {
  const w = world();
  w.write(T0);
  w.env.STATE.failPut = "R2 yazma hatası";
  await w.at(T0);
  assert.match(w.logs[0], /durum kaydedilemedi: R2 yazma hatası/);
  assert.equal(w.env.SERVER_A1.json(BEAT_KEY).state_error, "R2 yazma hatası");
});

test("a state that cannot be read stops the run instead of starting afresh", async () => {
  const w = world();
  w.write(T0);
  w.env.STATE.failGet = "R2 kapalı";
  await assert.rejects(w.at(T0), /R2 kapalı/);
  assert.equal(w.web.sent.length, 0);
});

test("heartbeats: every five minutes, at once when a verdict changes", async () => {
  const w = world();
  w.write(T0);
  await w.at(T0);
  await w.at(T0 + 3 * MIN);
  assert.equal(w.env.SERVER_A1.json(BEAT_KEY).time, new Date(T0).toISOString());
  await w.at(T0 + 5 * MIN);
  assert.equal(w.env.SERVER_A1.json(BEAT_KEY).time, new Date(T0 + 5 * MIN).toISOString());
  w.web.site("blog.example", 500);
  await w.at(T0 + 6 * MIN);
  assert.equal(w.env.SERVER_A1.json(BEAT_KEY).time, new Date(T0 + 6 * MIN).toISOString());
  assert.equal(w.env.SERVER_B2.json(BEAT_KEY).time, new Date(T0 + 5 * MIN).toISOString(), "B2 did not change");
  // A heartbeat that cannot be written is logged.
  w.env.SERVER_A1.failPut = "yazma reddedildi";
  await w.at(T0 + 11 * MIN);
  assert.match(w.logs.at(-1), /kalp atışı yazılamadı: A1: yazma reddedildi/);
});

test("more domains than the subrequests allow: all get their turn, no more than six at once", async () => {
  const w = world({ MAX_CHECKS: "10" });
  const doms = Array.from({ length: 25 }, (_, i) => `s${i}.example`);
  w.write(T0, { sites: [["cok", doms]] }, { sites: [] });
  const seen = new Set();
  for (let i = 0; i < 3; i++) {
    const out = await w.at(T0 + i * MIN);
    assert.ok(out.visit.length <= 10);
    out.visit.forEach((n) => seen.add(n));
  }
  assert.equal(seen.size, 25);
  assert.ok(w.web.maxOpen <= 6, `${w.web.maxOpen} visits at once`);
});

test("a visit refused for the subrequest limit is no failure: it goes first next time", async () => {
  const w = world();
  w.write(T0);
  w.web.site("blog.example", () => {
    throw new Error("Too many subrequests.");
  });
  await w.at(T0);
  const d = w.env.STATE.json(STATE_KEY).domains["blog.example"];
  assert.equal(d.fails, 0);
  assert.equal(d.waits, 1);
});

test("no server bindings: the run says so and does not fail", async () => {
  const env = { STATE: new FakeBucket(), TELEGRAM_TOKEN: TOKEN, TELEGRAM_CHAT: "1" };
  const web = new FakeWeb();
  await run(env, { now: T0, fetch: web.fetch, log: () => {} });
  assert.match(web.sent[0].text, /İzlenecek sunucu yok/);
  await assert.rejects(run({}, { now: T0, fetch: web.fetch, log: () => {} }), /STATE kovası bağlı değil/);
});
