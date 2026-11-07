// Stand-ins for the Worker's world: an R2 bucket binding and the web (the sites and
// Telegram's Bot API) behind fetch.

let etags = 0;

export class FakeBucket {
  constructor() {
    this.objects = new Map();
    this.gets = 0;
    this.puts = 0;
    this.failGet = "";
    this.failPut = "";
  }

  set(key, value, uploaded = Date.now()) {
    this.objects.set(key, { body: typeof value === "string" ? value : JSON.stringify(value), etag: `e${++etags}`, uploaded });
  }

  json(key) {
    const o = this.objects.get(key);
    return o ? JSON.parse(o.body) : undefined;
  }

  async get(key) {
    this.gets++;
    if (this.failGet) throw new Error(this.failGet);
    const o = this.objects.get(key);
    if (!o) return null;
    return { etag: o.etag, size: o.body.length, uploaded: new Date(o.uploaded), text: async () => o.body };
  }

  async put(key, value, opts = {}) {
    this.puts++;
    if (this.failPut) throw new Error(this.failPut);
    const cur = this.objects.get(key);
    if (opts.onlyIf && opts.onlyIf.etagMatches && (!cur || cur.etag !== opts.onlyIf.etagMatches)) return null;
    this.set(key, String(value));
    return { key, etag: this.objects.get(key).etag };
  }
}

/**
 * FakeWeb answers fetch: a site by its domain (status, a function, "timeout" or
 * "error"), Telegram's sendMessage at api.telegram.org. sent collects the messages.
 */
export class FakeWeb {
  constructor() {
    this.sites = new Map();
    this.sent = [];
    this.telegram = 200; // or a status to refuse with
    this.migrate = null; // {from, to}: that chat became a supergroup
    this.calls = 0;
    this.visits = [];
    this.open = 0;
    this.maxOpen = 0;
    this.fetch = this.fetch.bind(this);
  }

  site(domain, answer) {
    this.sites.set(domain, answer);
  }

  async fetch(input, init = {}) {
    const u = new URL(input);
    if (u.hostname === "api.telegram.org") {
      this.calls++;
      const asked = JSON.parse(init.body);
      if (this.migrate && String(asked.chat_id) === this.migrate.from) {
        return new Response(
          JSON.stringify({ ok: false, error_code: 400, description: "Bad Request: group chat was upgraded to a supergroup chat", parameters: { migrate_to_chat_id: Number(this.migrate.to) } }),
          { status: 400 },
        );
      }
      if (this.telegram !== 200) {
        return new Response(JSON.stringify({ ok: false, error_code: this.telegram, description: "Forbidden: bot was blocked by the user" }), {
          status: this.telegram,
        });
      }
      const body = JSON.parse(init.body);
      this.sent.push({ token: u.pathname.split("/")[1].slice(3), ...body });
      return new Response(JSON.stringify({ ok: true, result: { message_id: this.sent.length } }));
    }
    this.visits.push(u.hostname);
    this.open++;
    this.maxOpen = Math.max(this.maxOpen, this.open);
    try {
      let a = this.sites.has(u.hostname) ? this.sites.get(u.hostname) : 200;
      if (typeof a === "function") a = a(u, init);
      if (a && typeof a.then === "function") a = await a;
      // Let other visits start, as a network would.
      await new Promise((r) => setTimeout(r, 1));
      if (a === "timeout") {
        return await new Promise((_, reject) => {
          const fail = () => reject(new DOMException("The operation was aborted", "AbortError"));
          if (init.signal?.aborted) fail();
          init.signal?.addEventListener("abort", fail);
        });
      }
      if (a === "error") throw new TypeError("Network connection lost.");
      if (a instanceof Response) return a;
      const headers = a >= 300 && a < 400 ? { location: `https://${u.hostname}/` } : {};
      return new Response(a === 530 ? "error code: 1016" : "<html>ok</html>", { status: a, headers });
    } finally {
      this.open--;
    }
  }
}

/** status is a FootOps status file (internal/agent/statusfile.go); a string last_ok is
 * written as is (Go's own forms, such as its zero time). */
export function status(server, time, sites, backup = { ready: true, last_ok: time }) {
  const lastOK = typeof backup.last_ok === "string" ? backup.last_ok : backup.last_ok ? new Date(backup.last_ok).toISOString() : undefined;
  return {
    format: 1,
    server,
    footops: "test",
    time: new Date(time).toISOString(),
    every: 300,
    sites: sites.map(([name, domains, state = "running"]) => ({ name, domains, state })),
    backup: { ...backup, last_ok: lastOK },
  };
}

/** slow answers status after ms milliseconds. */
export const slow = (ms, answer = 522) => () => new Promise((r) => setTimeout(() => r(answer), ms));
