// FootOps' outside watcher (panel-tasarim §27.8, §27.17): a Cloudflare Worker on a cron.
// Each run reads every server's status file from its backup bucket, visits the sites'
// domains the way a visitor does and tells the servers' Telegram chat when a site stops
// answering, a server falls silent or its backups fall behind — and when that ends. It
// keeps what it knows in a bucket of its own and leaves a heartbeat next to each status
// file, so FootOps can tell when the watcher itself is gone or cannot send.
//
// It has no address: no fetch handler, and the example configuration turns workers.dev
// and preview URLs off, so nothing from outside can make it act.

import { check, pool } from "./check.js";
import {
  BEAT_KEY,
  SEND_MAX,
  STATE_KEY,
  STATUS_KEY,
  beat,
  beatDue,
  events,
  iso,
  observe,
  parseState,
  plan,
  record,
  render,
  serverCodes,
  settings,
  signature,
  spanTR,
  told,
  visitBudget,
} from "./logic.js";

// Workers allow six connections at once; more would queue and eat into the visits' own
// time limits.
const LANES = 6;
// A run must end before the next minute's begins (two at once could send the same news
// twice): no visit starts after 25 s (each lasts at most 15 s), no message after 45 s
// (each at most 5 s). Visits left out wait for the next run; they are not failures.
const VISITS_FOR = 25 * 1000;
const SENDS_UNTIL = 45 * 1000;
const SEND_TIMEOUT = 5 * 1000;
const JSON_TYPE = { httpMetadata: { contentType: "application/json" } };

export default {
  async scheduled(_controller, env) {
    await run(env);
  },
};

async function readStatus(bucket) {
  try {
    const obj = await bucket.get(STATUS_KEY);
    if (!obj) return { missing: true };
    if (obj.size > 1 << 20) {
      await obj.body?.cancel().catch(() => {});
      return { error: "1 MB'tan büyük" };
    }
    const file = JSON.parse(await obj.text());
    if (!file || typeof file !== "object" || !(Number(file.format) >= 1)) return { error: "biçimi tanınmadı" };
    return { file, uploaded: obj.uploaded instanceof Date ? obj.uploaded.getTime() : NaN };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}

// A state that cannot be read stops the run: starting afresh would greet the chat again
// and forget what it was told.
async function readState(bucket) {
  const obj = await bucket.get(STATE_KEY);
  if (!obj) return { state: parseState(null), etag: null };
  return { state: parseState(await obj.text()), etag: obj.etag };
}

// scrub keeps the bot token out of anything logged or written.
function scrub(s, token) {
  s = String(s);
  if (!token) return s;
  s = s.replaceAll(token, "<token>");
  const secret = token.split(":")[1];
  return secret && secret.length >= 8 ? s.replaceAll(secret, "<token>") : s;
}

// send is one sendMessage; a chat that became a supergroup says its new id.
async function send(cfg, token, chat, text, fetchFn) {
  const res = await fetchFn(`${cfg.telegramAPI}/bot${token}/sendMessage`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ chat_id: chat, text, link_preview_options: { is_disabled: true } }),
    signal: AbortSignal.timeout(SEND_TIMEOUT),
  });
  let body = {};
  try {
    body = await res.json();
  } catch {
    body = {};
  }
  if (res.ok && body.ok) return { ok: true };
  const to = body.parameters && body.parameters.migrate_to_chat_id;
  return {
    ok: false,
    migrateTo: to ? String(to) : "",
    error: `Telegram reddetti (HTTP ${res.status}${body.description ? ": " + String(body.description).slice(0, 160) : ""})`,
  };
}

/**
 * run is one round. io lets the tests bring their own clock, fetch, log and time for
 * visits; the Worker uses the real ones.
 */
export async function run(env, io = {}) {
  const started = Date.now();
  const now = io.now ?? started;
  const fetchFn = io.fetch ?? ((input, init) => fetch(input, init));
  const log = io.log ?? ((line) => console.log(line));
  const cfg = settings(env);
  if (!env.STATE || typeof env.STATE.get !== "function") {
    throw new Error('STATE kovası bağlı değil (wrangler.toml → [[r2_buckets]] binding = "STATE")');
  }
  const codes = serverCodes(env);
  const [saved, ...views] = await Promise.all([
    readState(env.STATE),
    ...codes.map(async (code) => ({ code, ...(await readStatus(env["SERVER_" + code])) })),
  ]);
  let st = saved.state;
  // Leaving a dry run starts afresh: the chat has heard nothing yet.
  if (st.dry && !cfg.dryRun) st = parseState(null);
  if (cfg.dryRun) st.dry = true;
  const before = JSON.stringify(st);

  observe(st, views, now, cfg);
  const sigs = Object.fromEntries(codes.map((c) => [c, signature(st, c)]));
  const { visit, due } = plan(st, now, cfg, visitBudget(cfg, codes.length));
  const results = await pool(visit, LANES, (name) => check(name, cfg, fetchFn, now), started + (io.visitsFor ?? VISITS_FOR));
  record(st, due, visit, results, now, cfg);

  const lines = [];
  visit.forEach((name, i) => {
    const r = results[i];
    if (!r) return;
    const d = st.domains[name];
    const where = d?.server || "?";
    if (!r.ok) lines.push(`  ✗ ${where} ${name} ${r.reason} (${d?.fails || 0}. hata${d?.down ? ", çalışmıyor" : ""})`);
    else if (cfg.dryRun || cfg.logChecks || r.note) lines.push(`  ✓ ${where} ${name} ${r.code} ${r.ms} ms${r.note ? " · " + r.note : ""}`);
  });

  // Tell the chat; a message that does not go stays for the next run.
  const msgs = render(events(st, now, cfg), st, cfg, now);
  const token = String(env.TELEGRAM_TOKEN || "");
  const chatSet = String(env.TELEGRAM_CHAT || "").trim();
  let sent = 0;
  let calls = 0;
  let sendErr = "";
  for (const m of msgs) {
    if (cfg.dryRun) {
      lines.push("  [kuru çalıştırma, gönderilmedi] " + m.text.replaceAll("\n", " ⏎ "));
      told(st, m.evs, now);
      sent++;
      continue;
    }
    if (!token || !chatSet) {
      sendErr = "TELEGRAM_TOKEN / TELEGRAM_CHAT tanımlı değil";
      break;
    }
    if (calls >= SEND_MAX || Date.now() - started > SENDS_UNTIL) break; // the rest next run
    // A chat that became a supergroup lives on under its new id (until the setting changes).
    const chat = st.chat && st.chat.from === chatSet ? st.chat.to : chatSet;
    try {
      calls++;
      let r = await send(cfg, token, chat, m.text, fetchFn);
      if (!r.ok && r.migrateTo && calls < SEND_MAX) {
        st.chat = { from: chatSet, to: r.migrateTo };
        calls++;
        r = await send(cfg, token, r.migrateTo, m.text, fetchFn);
      }
      if (!r.ok) {
        sendErr = r.error;
        break;
      }
    } catch (e) {
      sendErr = "Telegram'a ulaşılamadı: " + ((e && e.message) || e);
      break;
    }
    told(st, m.evs, now);
    sent++;
  }
  sendErr = scrub(sendErr, token);
  if (sendErr) st.sendFailing ??= iso(now);
  // Nothing left to tell: whatever failed before is no longer owed to anyone.
  else if (sent === msgs.length) delete st.sendFailing;

  // The state before the heartbeats: a beat must not say all is well when the state, and
  // with it what the chat was told, could not be kept.
  const beatsDue = codes.filter((c) => st.servers[c] && beatDue(st.servers[c], now, cfg, signature(st, c) !== sigs[c]));
  for (const c of beatsDue) st.servers[c].beat = iso(now);
  let stateErr = "";
  let raced = false;
  if (JSON.stringify(st) !== before) {
    st.time = iso(now);
    const opts = saved.etag ? { ...JSON_TYPE, onlyIf: { etagMatches: saved.etag } } : JSON_TYPE;
    try {
      // null: another run wrote in between; its view is as new as this one.
      raced = (await env.STATE.put(STATE_KEY, JSON.stringify(st), opts)) === null;
    } catch (e) {
      stateErr = String((e && e.message) || e).slice(0, 160);
    }
  }
  const trouble = { send: sendErr, state: stateErr };

  let beats = 0;
  const beatErrs = [];
  await Promise.all(
    beatsDue.map(async (code) => {
      try {
        await env["SERVER_" + code].put(BEAT_KEY, JSON.stringify(beat(st, code, now, cfg, trouble)), JSON_TYPE);
        beats++;
      } catch (e) {
        beatErrs.push(`${code}: ${(e && e.message) || e}`);
      }
    }),
  );

  const down = Object.values(st.domains).filter((d) => d.down && !d.gone).length;
  const visited = results.filter(Boolean);
  const bad = visited.filter((r) => !r.ok).length;
  const heard = codes
    .map((c) => {
      const srv = st.servers[c];
      const age = srv?.seen ? spanTR(now - Date.parse(srv.seen)) + " önce" : "hiç";
      return `${c} ${age}${srv?.silent ? " (sessiz)" : ""}${srv?.sitesError ? " (site listesi: " + srv.sitesError + ")" : ""}`;
    })
    .join(", ");
  const total = Object.values(st.domains).filter((d) => !d.gone).length;
  const left = due.length - visited.length;
  let head =
    `nöbetçi${cfg.dryRun ? " (kuru)" : ""}: ${codes.length} sunucu · ${visited.length}/${total} alan adı yoklandı` +
    `${left > 0 ? ` (${left} sonraki tura)` : ""}, ${bad} hatalı, ${down} çalışmıyor · haber: ${heard || "-"} · ` +
    `${sent}/${msgs.length} mesaj · ${beats} kalp atışı · ${Date.now() - started} ms`;
  if (sendErr) head += ` · mesaj gönderilemedi: ${sendErr}`;
  if (stateErr) head += ` · durum kaydedilemedi: ${stateErr}`;
  if (raced) head += " · durum kaydedilmedi (başka çalıştırma araya girdi)";
  if (beatErrs.length) head += ` · kalp atışı yazılamadı: ${beatErrs.join("; ")}`;
  for (const v of views) if (v.error) head += ` · ${v.code} durum dosyası okunamadı: ${v.error}`;
  log([head, ...lines].join("\n"));
  return { visit, due, results, msgs, sent, sendErr, stateErr, beats, state: st };
}
