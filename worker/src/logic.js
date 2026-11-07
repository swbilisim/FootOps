// What the watcher knows and what it says (panel-tasarim §27.8, §27.17): its settings,
// the state it keeps between runs, which domains a run visits, what the visits and the
// servers' status files mean, and the Telegram messages that come of it. No I/O here:
// index.js reads and writes, check.js visits.

export const STATE_FORMAT = 1;
// Next to the backups, written by each server's FootOps (internal/agent/statusfile.go).
export const STATUS_KEY = "izleme/durum.json";
// Next to it, written by the watcher: FootOps reads it to know the watcher is alive.
export const BEAT_KEY = "izleme/nobetci.json";
// In the watcher's own bucket.
export const STATE_KEY = "nobetci.json";

export const SEC = 1000;
export const MIN = 60 * SEC;
export const HOUR = 60 * MIN;

// Messages a run sends at most; the rest wait for the next run.
export const SEND_MAX = 3;
// Domains taken from one server's status file at most.
const MAX_DOMAINS = 500;
// Go writes a time never set as year 1; nothing real is that old.
const EPOCH = Date.UTC(2000, 0, 1);

// A host name as FootOps lists it: letters, digits and hyphens between dots, the last
// label starting with a letter (so no IP address).
const DOMAIN_RE = /^(?=.{4,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

// Telegram takes 4096 characters; FootOps keeps to the same margin.
const TELEGRAM_MAX = 4000;

/** Settings from the Worker's variables, each within sane bounds. */
export function settings(env = {}) {
  const num = (name, def, min, max) => {
    const raw = env[name];
    const v = raw === undefined || raw === "" ? NaN : Number(raw);
    return Number.isFinite(v) && v >= min && v <= max ? v : def;
  };
  const url = typeof env.CHECK_URL === "string" && env.CHECK_URL.includes("{domain}") ? env.CHECK_URL : "https://{domain}/";
  return {
    // A working domain is visited every so many minutes; one that fails, and every
    // other domain of its server, every minute until all is well again.
    every: Math.round(num("CHECK_EVERY_MIN", 2, 1, 60)),
    failAfter: Math.round(num("FAIL_AFTER", 3, 1, 20)),
    recoverAfter: Math.round(num("RECOVER_AFTER", 2, 1, 20)),
    // A server whose domains are still deciding holds its "down" news this long, so an
    // outage of the whole server is one message, not one a minute.
    hold: 2 * MIN,
    silentAfter: num("SILENT_AFTER_MIN", 15, 5, 24 * 60) * MIN,
    backupAfter: num("BACKUP_AFTER_H", 26, 2, 30 * 24) * HOUR,
    // At most 15 s: a run must end before the next minute's starts (index.js).
    timeout: num("TIMEOUT_SEC", 10, 2, 15) * SEC,
    maxChecks: Math.round(num("MAX_CHECKS", 40, 1, 1000)),
    // The plan's subrequests a run (Workers Free: 50; Paid allows more).
    subrequests: Math.round(num("SUBREQUESTS", 50, 20, 10000)),
    beatEvery: 5 * MIN,
    checkURL: url,
    telegramAPI: String(env.TELEGRAM_API || "https://api.telegram.org").replace(/\/+$/, ""),
    // Times in messages: minutes east of UTC (Türkiye: +3 h all year since 2016). A fixed
    // offset, not a zone: the first use of Intl costs a cron run ~8 ms of its 10 ms CPU.
    utcOffset: num("UTC_OFFSET_MIN", 180, -720, 840) * MIN,
    dryRun: /^(1|true|yes|evet)$/i.test(String(env.DRY_RUN ?? "")),
    logChecks: /^(1|true|yes|evet)$/i.test(String(env.LOG_CHECKS ?? "")),
  };
}

/**
 * visitBudget is how many domains a run may visit: the subrequests left after reading
 * the state and every status file, writing the state and every heartbeat, and sending
 * up to SEND_MAX messages.
 */
export function visitBudget(cfg, servers) {
  return Math.max(0, Math.min(cfg.maxChecks, cfg.subrequests - 2 * (1 + servers) - SEND_MAX));
}

/** The servers watched: one R2 binding named SERVER_<code> each, in code order. */
export function serverCodes(env = {}) {
  return Object.keys(env)
    .filter((k) => /^SERVER_[A-Z0-9_]{1,32}$/.test(k) && env[k] && typeof env[k].get === "function")
    .map((k) => k.slice("SERVER_".length))
    .sort();
}

export const iso = (ms) => new Date(ms).toISOString();
const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const clip = (s, n) => {
  s = String(s ?? "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
};
// clean keeps a name from a status file to what a name may hold: no line breaks or
// other marks that could pass for a message of its own in the chat.
export const clean = (s, n) => clip(String(s ?? "").replace(/[^\p{L}\p{N} ._-]/gu, "?"), n);
const ms = (s) => (s ? Date.parse(s) : NaN);

/** The state as read back (or a fresh one when there is none or it cannot be read). */
export function parseState(text) {
  let st = null;
  try {
    st = text ? JSON.parse(text) : null;
  } catch {
    st = null;
  }
  if (!isObj(st) || st.format !== STATE_FORMAT) st = { format: STATE_FORMAT };
  if (!isObj(st.servers)) st.servers = {};
  if (!isObj(st.domains)) st.domains = {};
  for (const [k, v] of Object.entries(st.servers)) if (!isObj(v)) delete st.servers[k];
  for (const [k, v] of Object.entries(st.domains)) if (!isObj(v)) delete st.domains[k];
  return st;
}

/** fnv1a: a domain's place among the minutes of the visit interval. */
export function slot(name, every) {
  let h = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h % every;
}

const failing = (d) => d.fails > 0 || d.down;
const live = (st, code) => Object.entries(st.domains).filter(([, d]) => d.server === code && !d.gone);

/**
 * observe takes in what the servers' status files say (views: {code, file, uploaded}
 * when read, {code, missing} when there is none, {code, error} when it could not be
 * read): when each server last spoke, how its backups stand and which domains it
 * serves. A server whose file cannot be read, or whose file says its site list could
 * not be read, keeps the domains it had.
 */
export function observe(st, views, now, cfg) {
  const codes = new Set(views.map((v) => v.code));
  for (const [code, srv] of Object.entries(st.servers)) if (!codes.has(code)) srv.gone = true;
  const listed = new Set();
  const keep = (code) => {
    for (const [name, d] of Object.entries(st.domains)) if (d.server === code) listed.add(name);
  };
  for (const v of views) {
    const srv = (st.servers[v.code] ??= {});
    delete srv.gone;
    // The tag is the binding's code: the watcher's own setting, not what a file says.
    srv.name = v.code;
    const f = v.file;
    // When it last spoke, by R2's clock (the server's own may be off).
    let last = ms(srv.seen);
    if (f) {
      const t = Number.isFinite(v.uploaded) ? v.uploaded : Date.parse(f.time);
      if (Number.isFinite(t)) last = t;
      delete srv.why;
    } else {
      srv.why = v.missing ? "Durum dosyası yok." : "Durum dosyası okunamadı: " + clip(v.error, 160);
    }
    if (Number.isFinite(last)) srv.seen = iso(last);
    const every = f && Number(f.every) > 0 ? Number(f.every) * SEC : 5 * MIN;
    const silent = Number.isFinite(last) ? now - last > Math.max(cfg.silentAfter, 3 * every) : Boolean(v.missing);
    if (silent && !srv.silent) {
      srv.silentFrom = Number.isFinite(last) ? iso(last) : null;
      delete srv.heardAt;
    }
    if (!silent && srv.silent) srv.heardAt = Number.isFinite(last) ? iso(last) : iso(now);
    srv.silent = silent;
    if (!f) {
      keep(v.code);
      continue;
    }

    const b = isObj(f.backup) ? f.backup : {};
    const ok = Date.parse(b.last_ok);
    srv.lastOK = Number.isFinite(ok) && ok > EPOCH ? iso(ok) : null;
    srv.backup = b.ready !== true ? "off" : b.paused === true ? "paused" : "on";
    srv.late = srv.backup === "on" && srv.lastOK !== null && now - ok > cfg.backupAfter;

    if (f.sites_error || !Array.isArray(f.sites)) {
      srv.sitesError = clip(f.sites_error || "site listesi yok", 160);
      keep(v.code);
      continue;
    }
    delete srv.sitesError;
    let n = 0;
    for (const site of f.sites) {
      // Parked sites (Faz P) are switched off on purpose.
      if (!isObj(site) || site.state === "parked") continue;
      for (const raw of Array.isArray(site.domains) ? site.domains : []) {
        const name = String(raw).trim().toLowerCase().replace(/\.$/, "");
        if (!DOMAIN_RE.test(name) || listed.has(name) || n >= MAX_DOMAINS) continue; // two servers: the first keeps it
        listed.add(name);
        n++;
        const d = (st.domains[name] ??= { fails: 0 });
        d.server = v.code;
        d.site = clean(site.name, 64);
        d.container = clean(site.state, 16);
        delete d.gone;
      }
    }
  }
  for (const [name, d] of Object.entries(st.domains)) {
    if (listed.has(name)) continue;
    // Gone from the lists: forgotten, unless the chat was told of it.
    if (d.told || d.toldNote) d.gone = true;
    else delete st.domains[name];
  }
  for (const [code, srv] of Object.entries(st.servers)) {
    if (srv.gone && !srv.toldSilent && !srv.toldLate) delete st.servers[code];
  }
}

/**
 * plan picks the domains this run visits: those whose turn it is, those a run before
 * could not get to, and every domain of a server that has one failing (so a server that
 * went down is seen whole, at once). Servers take turns in the list, so one whose
 * visits hang cannot crowd out the others. Returns {visit, due}: what to visit within
 * the budget, and all that was due (what is left out goes first next time).
 */
export function plan(st, now, cfg, budget = cfg.maxChecks) {
  const minute = Math.floor(now / MIN);
  const byServer = new Map();
  for (const n of Object.keys(st.domains).sort()) {
    const d = st.domains[n];
    if (d.gone) continue;
    if (!byServer.has(d.server)) byServer.set(d.server, []);
    byServer.get(d.server).push(n);
  }
  // Those waiting longest first, then the failing ones: every domain gets its turn.
  const order = (a, b) => {
    const x = st.domains[a];
    const y = st.domains[b];
    return (y.waits || 0) - (x.waits || 0) || Number(failing(y)) - Number(failing(x)) || (a < b ? -1 : 1);
  };
  const queues = [...byServer.keys()].sort().map((code) => {
    const names = byServer.get(code);
    const alarmed = names.some((n) => failing(st.domains[n]));
    return names.filter((n) => alarmed || st.domains[n].waits || slot(n, cfg.every) === minute % cfg.every).sort(order);
  });
  const visit = [];
  for (let i = 0; visit.length < budget && queues.some((q) => i < q.length); i++) {
    for (const q of queues) if (i < q.length && visit.length < budget) visit.push(q[i]);
  }
  return { visit, due: queues.flat() };
}

/**
 * record takes in the visits (a result missing: not visited — out of time or budget):
 * FAIL_AFTER failures in a row make a domain down, RECOVER_AFTER answers in a row make
 * it well again; a single failure in between is a blip and forgotten at the next
 * answer. A domain due but not visited goes first next time.
 */
export function record(st, due, visit, results, now, cfg) {
  const got = new Map();
  visit.forEach((name, i) => results[i] && got.set(name, results[i]));
  for (const name of due) {
    const d = st.domains[name];
    if (!d) continue;
    const r = got.get(name);
    if (!r) {
      d.waits = (d.waits || 0) + 1;
      continue;
    }
    delete d.waits;
    if (r.note) d.note = r.note;
    else delete d.note;
    if (r.ok) answered(d, now, cfg);
    else failed(d, r, now, cfg);
  }
}

function answered(d, now, cfg) {
  if (d.down) {
    if (!d.oks) d.okAt = iso(now);
    d.oks = (d.oks || 0) + 1;
    if (d.oks < cfg.recoverAfter) return;
    d.down = false;
    delete d.downAt;
  }
  d.fails = 0;
  delete d.oks;
  delete d.code;
  delete d.reason;
  delete d.nextSince;
  // What the chat was told down keeps its start and end for the "back" message.
  if (!d.told) {
    delete d.since;
    delete d.okAt;
  } else d.okAt ??= iso(now);
}

function failed(d, r, now, cfg) {
  if (d.down) {
    // The recovery did not hold.
    delete d.oks;
    delete d.okAt;
  }
  if (!d.fails) {
    // Back, as far as the watcher knows, but the chat not told yet: the outage it was
    // told of ended (okAt stays), and a new run of failures starts.
    if (d.told && !d.down) d.nextSince = iso(now);
    else if (!(d.told && d.since)) d.since = iso(now);
  }
  d.fails = (d.fails || 0) + 1;
  d.code = clip(r.code, 16);
  d.reason = clip(r.reason, 200);
  if (!d.down && d.fails >= cfg.failAfter) {
    d.down = true;
    d.downAt = iso(now);
    // Down again before the chat heard it was back: for the chat it never ended.
    if (d.told) {
      delete d.nextSince;
      delete d.okAt;
    }
  }
}

/**
 * events is what the chat should hear: the difference between what the watcher now
 * judges and what the chat was told. Down news of a server waits while other domains of
 * it are still deciding (at most cfg.hold), so a whole server going is one message.
 */
export function events(st, now, cfg) {
  const out = [];
  if (!st.welcomed) out.push({ kind: "welcome" });
  for (const [code, srv] of Object.entries(st.servers)) {
    if (srv.gone) {
      out.push({ kind: "serverGone", code });
      continue;
    }
    if (srv.silent && !srv.toldSilent) out.push({ kind: "silent", code });
    if (!srv.silent && srv.toldSilent) out.push({ kind: "heard", code });
    if (srv.late && !srv.toldLate && !srv.silent) out.push({ kind: "late", code });
    if (!srv.late && srv.toldLate) out.push({ kind: "backupOK", code });
  }
  const held = new Set();
  for (const code of Object.keys(st.servers)) {
    const doms = live(st, code).map(([, d]) => d);
    const deciding = doms.some((d) => d.fails > 0 && !d.down);
    const firstDown = Math.min(...doms.filter((d) => d.down && !d.told).map((d) => ms(d.downAt) || now));
    if (deciding && now - firstDown < cfg.hold) held.add(code);
  }
  for (const [name, d] of Object.entries(st.domains).sort(([a], [b]) => (a < b ? -1 : 1))) {
    if (d.gone) {
      out.push({ kind: "gone", name });
      continue;
    }
    if (d.down && !d.told && !held.has(d.server)) out.push({ kind: "down", name });
    else if (!d.down && d.told) out.push({ kind: "up", name });
    if (d.note && !d.toldNote) out.push({ kind: "blind", name });
    else if (!d.note && d.toldNote) out.push({ kind: "seen", name });
  }
  return out;
}

/** told records that the chat heard evs. */
export function told(st, evs, now) {
  for (const e of evs) {
    const srv = e.code !== undefined ? st.servers[e.code] : undefined;
    const d = e.name !== undefined ? st.domains[e.name] : undefined;
    switch (e.kind) {
      case "welcome":
        st.welcomed = iso(now);
        break;
      case "silent":
        if (srv) srv.toldSilent = true;
        break;
      case "heard":
        if (srv) {
          srv.toldSilent = false;
          delete srv.silentFrom;
          delete srv.heardAt;
        }
        break;
      case "late":
        if (srv) srv.toldLate = true;
        break;
      case "backupOK":
        if (srv) srv.toldLate = false;
        break;
      case "serverGone":
        delete st.servers[e.code];
        break;
      case "down":
        if (d) d.told = true;
        break;
      case "up":
        if (d) {
          d.told = false;
          // Failing again since it came back: that run of failures keeps its start.
          if (d.fails > 0) d.since = d.nextSince || iso(now);
          else delete d.since;
          delete d.okAt;
          delete d.nextSince;
        }
        break;
      case "blind":
        if (d) d.toldNote = true;
        break;
      case "seen":
        if (d) d.toldNote = false;
        break;
      case "gone":
        delete st.domains[e.name];
        break;
    }
  }
}

// ---- messages ----

/** spanTR is a duration in Turkish, as FootOps writes it: "3 dk", "2 sa 5 dk", "3 gün 4 sa". */
export function spanTR(d) {
  const m = Math.floor(Math.max(d, 0) / MIN);
  if (m < 60) return `${Math.max(m, 1)} dk`;
  if (m < 24 * 60) return m % 60 === 0 ? `${m / 60} sa` : `${Math.floor(m / 60)} sa ${m % 60} dk`;
  const h = Math.floor(m / 60);
  return h % 24 === 0 ? `${h / 24} gün` : `${Math.floor(h / 24)} gün ${h % 24} sa`;
}

/** localTime is "07.11 16:05" at the given offset from UTC ("?" for no time). */
export function localTime(t, offset = 0) {
  if (!Number.isFinite(t)) return "?";
  const s = new Date(t + offset).toISOString();
  return `${s.slice(8, 10)}.${s.slice(5, 7)} ${s.slice(11, 16)}`;
}

const CONTAINER = {
  stopped: "container durmuş",
  missing: "container yok",
  restarting: "container sürekli yeniden başlıyor",
  unhealthy: "container sağlıksız",
  paused: "container dondurulmuş",
};

const plural = (n, word) => `${n} ${word}`;

/** subject names what a group of domains of one server is: a domain, a site, or a count. */
function subject(st, code, names) {
  const doms = live(st, code);
  const sites = new Set(names.map((n) => st.domains[n]?.site));
  const allSites = new Set(doms.map(([, d]) => d.site));
  if (names.length >= 2 && names.length === doms.length) {
    return { text: `Sitelerin hepsi (${plural(allSites.size, "site")}, ${plural(names.length, "alan adı")})`, list: false };
  }
  if (names.length === 1) return { text: names[0], list: false };
  if (sites.size === 1) {
    const site = [...sites][0];
    const siteDoms = doms.filter(([, d]) => d.site === site);
    if (site && siteDoms.length === names.length) return { text: `${site} (${plural(names.length, "alan adı")})`, list: true };
  }
  return { text: plural(names.length, "alan adı"), list: true };
}

/** group splits events by a key, keeping the order in which the keys first came. */
function group(evs, key) {
  const m = new Map();
  for (const e of evs) {
    const k = key(e);
    if (!m.has(k)) m.set(k, []);
    m.get(k).push(e);
  }
  return [...m.values()];
}

/**
 * render turns events into Telegram messages: problems first, then what got better,
 * domains of one server and one cause in one line. Each message carries the events it
 * tells, so a message that does not go leaves them for the next run.
 */
export function render(evs, st, cfg, now) {
  const blocks = [];
  const tag = (code) => `[${st.servers[code]?.name || code}]`;
  const kind = (k) => evs.filter((e) => e.kind === k);
  const at = (s) => localTime(ms(s), cfg.utcOffset);
  const dom = (e) => st.domains[e.name];

  for (const e of kind("welcome")) {
    const lines = ["🟢 FootOps nöbetçisi çalışıyor."];
    const per = Object.keys(st.servers)
      .filter((c) => !st.servers[c].gone)
      .sort()
      .map((c) => {
        const doms = live(st, c);
        return `${st.servers[c].name || c}: ${plural(new Set(doms.map(([, d]) => d.site)).size, "site")}, ${plural(doms.length, "alan adı")}`;
      });
    if (per.length) lines.push(per.join(" · "));
    else lines.push("İzlenecek sunucu yok: Worker ayarlarında SERVER_<kod> kova bağı tanımlı değil.");
    const often = cfg.every === 1 ? "dakikada bir" : `${cfg.every} dakikada bir`;
    lines.push(
      `Her alan adı ${often} ziyaretçi gibi açılır; ${cfg.failAfter} kez üst üste açılmazsa ❌, düzelince ✅. ` +
        `FootOps ${Math.round(cfg.silentAfter / MIN)} dakika susarsa 🔴, son yedeğin üzerinden ` +
        `${Math.round(cfg.backupAfter / HOUR)} saat geçerse 🟡.`,
    );
    blocks.push({ text: lines.join("\n"), evs: [e] });
  }

  for (const e of kind("silent")) {
    const srv = st.servers[e.code];
    const lines = [
      srv.silentFrom
        ? `🔴 ${tag(e.code)} FootOps'tan ${spanTR(now - ms(srv.silentFrom))} haber yok (son haber ${at(srv.silentFrom)})`
        : `🔴 ${tag(e.code)} FootOps'tan haber yok`,
    ];
    if (srv.why) lines.push(srv.why);
    const doms = live(st, e.code).map(([, d]) => d);
    const bad = doms.filter(failing).length;
    if (doms.length && bad === doms.length) lines.push("Sitelerine de ulaşılamıyor: sunucu kapalı ya da ağı gitmiş olabilir.");
    else if (doms.length && bad === 0) lines.push("Siteleri açılıyor: sunucu çalışıyor; FootOps'un agent'ı durmuş ya da yedek kovasına yazamıyor olabilir.");
    else if (bad) lines.push(`${bad} / ${plural(doms.length, "alan adı")} da açılmıyor.`);
    blocks.push({ text: lines.join("\n"), evs: [e] });
  }

  // Down: one line per server, cause and container state.
  for (const list of group(kind("down"), (e) => `${dom(e).server}\u0000${dom(e).reason}\u0000${CONTAINER[dom(e).container] || ""}`)) {
    const d0 = dom(list[0]);
    const names = list.map((e) => e.name);
    const s = subject(st, d0.server, names);
    const extra = CONTAINER[d0.container] ? ` · ${CONTAINER[d0.container]}` : "";
    let text = `❌ ${tag(d0.server)} ${s.text} çalışmıyor: ${d0.reason}${extra}`;
    if (s.list) text += "\n" + names.join(", ");
    blocks.push({ text, evs: list });
  }

  for (const list of group(kind("blind"), (e) => dom(e).server)) {
    const d0 = dom(list[0]);
    blocks.push({
      text:
        `⚠️ ${tag(d0.server)} ${list.map((e) => e.name).join(", ")} izlenemiyor: Cloudflare ziyareti bir doğrulama sayfasıyla ` +
        "karşılıyor (Bot Fight Mode ya da bir güvenlik kuralı), siteye ulaşılıp ulaşılmadığı görülemiyor.",
      evs: list,
    });
  }

  for (const e of kind("heard")) {
    const srv = st.servers[e.code];
    const took = srv.silentFrom && srv.heardAt ? ` (sessizlik ${spanTR(ms(srv.heardAt) - ms(srv.silentFrom))} sürdü)` : "";
    blocks.push({ text: `✅ ${tag(e.code)} FootOps'tan yeniden haber var${took}`, evs: [e] });
  }

  const took = (d) => (d.since && d.okAt ? spanTR(ms(d.okAt) - ms(d.since)) : "");
  for (const list of group(kind("up"), (e) => `${dom(e).server}\u0000${took(dom(e))}`)) {
    const d0 = dom(list[0]);
    const names = list.map((e) => e.name);
    const s = subject(st, d0.server, names);
    let text = `✅ ${tag(d0.server)} ${s.text} yeniden açılıyor${took(d0) ? ` (kesinti ${took(d0)})` : ""}`;
    if (s.list) text += "\n" + names.join(", ");
    blocks.push({ text, evs: list });
  }

  for (const list of group(kind("seen"), (e) => dom(e).server)) {
    blocks.push({ text: `✅ ${tag(dom(list[0]).server)} ${list.map((e) => e.name).join(", ")} yeniden izlenebiliyor.`, evs: list });
  }

  for (const e of kind("late")) {
    const srv = st.servers[e.code];
    blocks.push({
      text:
        `🟡 ${tag(e.code)} Son başarılı yedek ${spanTR(now - ms(srv.lastOK))} önce (${at(srv.lastOK)}): ` +
        "yedekler alınmıyor olabilir — panelde Yedekleme sayfasına bak.",
      evs: [e],
    });
  }
  for (const e of kind("backupOK")) {
    const srv = st.servers[e.code];
    let text = `✅ ${tag(e.code)} Yedek gecikmesi uyarısı kalktı.`;
    if (srv.backup === "paused") text = `✅ ${tag(e.code)} Yedek gecikmesi uyarısı kalktı: yedekleme duraklatıldı.`;
    else if (srv.backup === "off") text = `✅ ${tag(e.code)} Yedek gecikmesi uyarısı kalktı: yedek deposu bağlı değil.`;
    else if (srv.lastOK) text = `✅ ${tag(e.code)} Yedekler yeniden alınıyor (son yedek ${at(srv.lastOK)})`;
    blocks.push({ text, evs: [e] });
  }

  for (const e of kind("gone")) {
    const d = dom(e);
    const was = d.told ? "; çalışmıyor diye bildirilmişti" : "";
    blocks.push({ text: `⚪ ${tag(d.server)} ${e.name} artık izlenmiyor (FootOps'un site listesinde yok)${was}.`, evs: [e] });
  }
  for (const e of kind("serverGone")) {
    blocks.push({ text: `⚪ ${tag(e.code)} artık izlenmiyor (nöbetçinin ayarlarından çıkarıldı).`, evs: [e] });
  }
  return pack(blocks);
}

/** pack joins blocks into messages of at most TELEGRAM_MAX characters. */
export function pack(blocks, max = TELEGRAM_MAX) {
  const out = [];
  let cur = null;
  for (const b of blocks) {
    const text = clip(b.text, max);
    if (cur && cur.text.length + 2 + text.length > max) {
      out.push(cur);
      cur = null;
    }
    if (cur) {
      cur.text += "\n\n" + text;
      cur.evs.push(...b.evs);
    } else cur = { text, evs: [...b.evs] };
  }
  if (cur) out.push(cur);
  return out;
}

// ---- the heartbeat in each server's bucket ----

/** signature of a server's domains as the watcher judges them: a beat goes when it changes. */
export function signature(st, code) {
  return live(st, code)
    .map(([n, d]) => `${n}:${d.fails > 0 ? 1 : 0}${d.down ? 1 : 0}${d.note ? 1 : 0}`)
    .sort()
    .join(",");
}

/**
 * beat is izleme/nobetci.json for one server (FootOps' panel shows it; no secrets).
 * trouble is this run's: a message that could not be sent, a state that could not be
 * saved — FootOps raises an alert when either lasts.
 */
export function beat(st, code, now, cfg, trouble = {}) {
  const out = {
    format: 1,
    time: iso(now),
    every: cfg.beatEvery / SEC,
    interval: cfg.every * 60,
    domains: live(st, code)
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([name, d]) => {
        const o = { name, site: d.site || "", ok: !failing(d) };
        if (d.down) o.down = true;
        if (d.code) o.code = d.code;
        if (d.reason) o.reason = d.reason;
        if (failing(d) && d.since) o.since = d.since;
        if (d.note) o.note = d.note;
        return o;
      }),
  };
  if (trouble.send) out.send_error = trouble.send;
  if (st.sendFailing) out.send_failing = st.sendFailing;
  if (trouble.state) out.state_error = trouble.state;
  return out;
}

/** beatDue: the last beat is old enough, or what the watcher judges changed. */
export function beatDue(srv, now, cfg, changed) {
  const last = ms(srv?.beat);
  // A run comes a little late now and then: half a minute of slack.
  return changed || !Number.isFinite(last) || now - last >= cfg.beatEvery - 30 * SEC;
}
