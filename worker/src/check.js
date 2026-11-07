// Visiting a domain the way a visitor does: through Cloudflare (DNS, its SSL mode and the
// origin are tried together), the home page, redirects not followed. A 5xx (Caddy's
// 502, Cloudflare's 52x), no answer in time or no connection at all is a failure;
// anything else is an answer — a redirect, a login page, a 404, a Cloudflare challenge.

const UA = "Mozilla/5.0 (compatible; FootOps-Nobetci/1; +https://github.com/swbilisim/FootOps)";

const STATUS = {
  500: "sitede sunucu hatası",
  502: "site yanıt vermiyor",
  503: "site geçici olarak hizmet dışı",
  504: "site zamanında yanıt vermedi",
  520: "sunucudan beklenmeyen yanıt",
  521: "sunucu bağlantıyı reddetti",
  522: "sunucuya bağlanılamadı (kapalı ya da ağı yok)",
  523: "sunucuya ulaşılamıyor",
  524: "sunucu zamanında yanıt vermedi",
  525: "sunucuyla SSL kurulamadı",
  526: "sunucunun SSL sertifikası geçersiz",
  530: "Cloudflare sunucuya ulaşamadı",
};

// Cloudflare's own error codes, in the body of a 530 ("error code: 1016").
const CF_CODE = {
  1016: "alan adının köken DNS kaydı çözülemiyor",
  1033: "Cloudflare Tunnel bağlı değil",
};

/** url is the address visited: CHECK_URL with the domain, and a cache-buster so no
 * cache rule can answer for the origin. */
export function url(cfg, domain, now) {
  const base = cfg.checkURL.replaceAll("{domain}", domain);
  return base + (base.includes("?") ? "&" : "?") + "footops-nobetci=" + Math.floor(now / 60000);
}

/** reason is a status code's few words. */
export function statusReason(status, cfCode) {
  let text = `${status} — ${STATUS[status] || "sunucu hatası"}`;
  if (cfCode) text += ` (Cloudflare ${cfCode}${CF_CODE[cfCode] ? ": " + CF_CODE[cfCode] : ""})`;
  return text;
}

async function readSome(res, max) {
  if (!res.body) return "";
  const reader = res.body.getReader();
  const parts = [];
  let n = 0;
  try {
    while (n < max) {
      const { done, value } = await reader.read();
      if (done) break;
      parts.push(value);
      n += value.byteLength;
    }
  } finally {
    reader.cancel().catch(() => {});
  }
  const all = new Uint8Array(Math.min(n, max));
  let off = 0;
  for (const p of parts) {
    const take = Math.min(p.byteLength, all.length - off);
    all.set(p.subarray(0, take), off);
    off += take;
    if (off >= all.length) break;
  }
  return new TextDecoder().decode(all);
}

/**
 * check visits one domain. The result: {ok, code, reason, note, ms}; code is the HTTP
 * status, "timeout" or "error". Its time limit starts when the request does. null: the
 * Worker may not make another request this run (its subrequest limit) — not a failure
 * of the site; it is visited next time.
 */
export async function check(domain, cfg, fetchFn, now) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), cfg.timeout);
  const start = Date.now();
  try {
    const res = await fetchFn(url(cfg, domain, now), {
      method: "GET",
      redirect: "manual",
      signal: ctrl.signal,
      headers: { "user-agent": UA, accept: "text/html,application/xhtml+xml,*/*;q=0.8" },
    });
    const took = Date.now() - start;
    if (res.status >= 500) {
      let cf = "";
      if (res.status === 530) {
        // The status is the answer; the body only adds Cloudflare's code, if it comes.
        const body = await readSome(res, 4096).catch(() => "");
        cf = (/error code:?\s*(\d{4})/i.exec(body) || [])[1] || "";
      } else await res.body?.cancel().catch(() => {});
      return { ok: false, code: String(res.status), reason: statusReason(res.status, cf), ms: took };
    }
    await res.body?.cancel().catch(() => {});
    // A challenge is Cloudflare answering for the site: it is up, but the visit did not
    // reach it: the chat hears the domain cannot be watched (logic.js, "blind").
    const mitigated = res.headers.get("cf-mitigated");
    return { ok: true, code: String(res.status), note: mitigated ? `cloudflare-${mitigated}` : "", ms: took };
  } catch (e) {
    const took = Date.now() - start;
    if (ctrl.signal.aborted) {
      return { ok: false, code: "timeout", reason: `zaman aşımı (${Math.round(cfg.timeout / 1000)} sn içinde yanıt yok)`, ms: took };
    }
    const msg = String((e && e.message) || e).slice(0, 120);
    if (/too many subrequests/i.test(msg)) return null;
    return { ok: false, code: "error", reason: `bağlantı kurulamadı (${msg})`, ms: took };
  } finally {
    clearTimeout(timer);
  }
}

/**
 * pool runs fn over items, at most size at a time, in order of the items. After the
 * deadline (a Date.now() value) nothing new starts: those items are left undefined.
 */
export async function pool(items, size, fn, deadline = Infinity) {
  const out = new Array(items.length);
  let next = 0;
  const lanes = Array.from({ length: Math.min(size, items.length) }, async () => {
    while (next < items.length && Date.now() < deadline) {
      const i = next++;
      out[i] = await fn(items[i], i);
    }
  });
  await Promise.all(lanes);
  return out;
}
