# FootOps outside watcher (`footops-nobetci`)

A Cloudflare Worker that watches FootOps servers from outside, the way a visitor would.
FootOps tells its Telegram chat about what goes wrong inside a server (backups, disk,
certificates, MongoDB…); it cannot say that the server itself is gone, or that a site no
longer opens from the internet. The watcher does, in the same chat:

| Message | When |
|---|---|
| `❌ [H1] example.com çalışmıyor: 502 — site yanıt vermiyor` | a domain fails three visits in a row (5xx, Cloudflare 52x/530, no answer in 10 s, no connection) |
| `✅ [H1] example.com yeniden açılıyor (kesinti 6 dk)` | it answers twice in a row |
| `🔴 [H1] FootOps'tan 16 dk haber yok` | the server's status file is older than 15 minutes; the second line says whether its sites still open |
| `🟡 [H1] Son başarılı yedek 1 gün 3 sa önce` | the newest backup is older than 26 hours (a second look; FootOps reports its own backup failures) |
| `⚠️ [H1] example.com izlenemiyor` | Cloudflare answers the visit with a challenge (Bot Fight Mode, a security rule): the site cannot be watched |
| `🟢 FootOps nöbetçisi çalışıyor.` | the first run |

Domains of one server and one cause go in one line; a whole server going down is one
message. A redirect, a login page, a 404 or a Cloudflare challenge is an answer.

## How it works

- Every server's FootOps writes `izleme/durum.json` to its backup bucket every 5
  minutes: its sites, their domains, container states and the latest backup (no
  secrets). The watcher reads it through an R2 binding — no keys.
- A cron runs the Worker every minute. A working domain is visited every
  `CHECK_EVERY_MIN` minutes (spread over the minutes); once one fails, every domain of
  that server is visited every minute until all is well. Servers take turns for the six
  connections a run has, so one whose visits hang cannot crowd out the others; a domain
  left out (no time or subrequests left) goes first next run. Silence is judged by R2's
  clock (when the file arrived), not the server's.
- A run ends well inside its minute: no visit starts after 25 s, a visit lasts at most
  `TIMEOUT_SEC` (≤ 15 s), at most 3 messages go, the last no later than 45 s in. The
  rest waits for the next run; two runs at once could send the same news twice.
- The watcher writes `izleme/nobetci.json` next to each status file every 5 minutes (and
  at once when a verdict changes). FootOps reads it: its panel shows what the watcher
  sees, and it raises an alert (to Telegram) when the watcher has been silent for 30
  minutes, or has failed to send its messages or keep its state for 15. A chat that
  became a supergroup is followed to its new id. If you take the watcher down for good,
  delete `izleme/nobetci.json` from the buckets, or FootOps keeps saying it is silent.
- What it knows (which domain is down, what the chat was told) lives in a bucket of its
  own, `nobetci.json`. It has no address: no fetch handler, workers.dev and preview
  URLs off.

## Set up

Requires a Cloudflare account (the free Workers plan is enough) and FootOps servers with
their backups on R2 in that account.

1. Create the watcher's bucket: `npx wrangler@4.152.0 r2 bucket create footops-izleme`
2. `cp wrangler.example.toml wrangler.toml` (git ignores it) and list one
   `SERVER_<code>` binding per server, its backup bucket as `bucket_name`.
3. Secrets: `npx wrangler@4.152.0 secret put TELEGRAM_TOKEN` (the servers' bot) and
   `… secret put TELEGRAM_CHAT` (the chat's id).
4. `npx wrangler@4.152.0 deploy`. To try it first, deploy with `DRY_RUN = "1"`: messages
   go to the log (`npx wrangler@4.152.0 tail`) instead of Telegram. Turning it off later
   starts afresh, with the greeting.

## Settings (`[vars]`)

| Variable | Default | |
|---|---|---|
| `CHECK_EVERY_MIN` | `2` | minutes between visits of a working domain (1-60) |
| `FAIL_AFTER` | `3` | failures in a row before ❌ |
| `RECOVER_AFTER` | `2` | answers in a row before ✅ |
| `SILENT_AFTER_MIN` | `15` | status file age before 🔴 (at least three of FootOps' intervals) |
| `BACKUP_AFTER_H` | `26` | newest backup age before 🟡 (not while backups are paused) |
| `TIMEOUT_SEC` | `10` | a visit with no answer by then fails (2-15) |
| `MAX_CHECKS` | `40` | visits per run at most; more domains take turns |
| `SUBREQUESTS` | `50` | the plan's subrequests a run (Workers Free); R2 calls and up to 3 messages are taken off before visits |
| `TIME_ZONE` | `Europe/Istanbul` | times in messages |
| `DRY_RUN`, `LOG_CHECKS` | off | log messages instead of sending; log every visit |

## Cost

Free plan: one invocation a minute (1,440 a day of 100,000), a few milliseconds of CPU.
R2: about 3 reads a minute and a few writes every 5 minutes (well inside the free 10 M
reads and 1 M writes a month). Each visit costs the server about what a visitor's page
view does; with 16 domains every 2 minutes, measured on a 2-core server: about 0.07% of
its CPU and 2-3 GB of traffic a month.

## Development

No dependencies: `npm test` (Node 22+, `node:test`). Locally, under the real runtime:
`npx wrangler@4.152.0 dev --test-scheduled` with `CHECK_URL` and `TELEGRAM_API` pointing
at stand-ins, then `curl "http://localhost:8787/__scheduled?cron=*+*+*+*+*"`.
