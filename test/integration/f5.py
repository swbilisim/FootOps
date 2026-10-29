#!/usr/bin/env python3
"""F5 scenarios (database browser) against a real install, after f0.py (itsite and its
200.015 documents): collections and sizes, pages of a 200 000-document collection,
filters (an operator running JavaScript refused), one document as text; an edit through
the real panel (login, CSRF, the multipart form, the diff, save) with every type kept;
a change by the app in between (nothing written); delete and undo, undo of an edit,
byte for byte; a job on the site holding changes off; no process or container started
for any of it.

Usage: f5.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import re
import sys
import time
import urllib.parse

from itlib import ACTOR, SITE, api, fail, fresh_totp, ok, rss, sh, tools, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
JAR = "/tmp/f5-jar"
measure = {}

uri = re.search(r'MONGODB_URI: "([^"]+)"', sh("cat /srv/musteri/%s/compose.yaml" % SITE)).group(1)
local = uri.replace("@mongo:27017", "@127.0.0.1:27017")


def site_js(js):
    out = sh("docker exec -i footops-mongo mongosh --quiet '%s' --file /dev/stdin" % local, stdin=js.encode())
    return out.strip()


def canonical(coll, id_js):
    """The stored document as Extended JSON. mongosh reads numbers as JavaScript numbers,
    so this does not tell int32 from double: types() does, and stored() compares bytes."""
    out = site_js('print("D "+EJSON.stringify(db.getCollection(%s).findOne({_id: %s}), {relaxed: false}))' % (json.dumps(coll), id_js))
    return out.split("D ", 1)[1]


def types(coll, id_js):
    """Each field's type as the server itself reports it ($type)."""
    out = site_js('const d=db.getCollection(%s).aggregate([{$match: {_id: %s}}, {$project: {_id: 0, t: {$map: '
                  '{input: {$objectToArray: "$$ROOT"}, as: "e", in: {k: "$$e.k", t: {$type: "$$e.v"}}}}}}]).toArray()[0];'
                  'print("T "+JSON.stringify(Object.fromEntries(d.t.map(x => [x.k, x.t]))))' % (json.dumps(coll), id_js))
    return json.loads(out.split("T ", 1)[1])


def stored(coll, id_text):
    """The sha256 of the stored document's bytes (the agent hashes them as the server sent them)."""
    code, d = api("GET", q("/v1/sites/%s/db/doc" % SITE, c=coll, id=id_text), check=False)
    return d["hash"] if code == 200 else None


def q(path, **params):
    return path + "?" + urllib.parse.urlencode(params)


def timed(method, path, body=None):
    t0 = time.time()
    code, r = api(method, path, body, check=False)
    return code, r, round((time.time() - t0) * 1000)


def web(path, form=None, multipart=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar."""
    args = ""
    for k, v in (form or {}).items():
        args += " --data-urlencode %s" % quote("%s=%s" % (k, v))
    for k, v in (multipart or {}).items():
        args += " -F %s" % quote("%s=%s" % (k, v)) if v.startswith("<") else " --form-string %s" % quote("%s=%s" % (k, v))
    out = sh("curl -sS -b %s -c %s -o /tmp/f5-page -w '%%{http_code} %%{redirect_url}' %s 'http://localhost:7701%s'; echo; cat /tmp/f5-page"
             % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, _, loc = head.partition(" ")
    return int(code), loc.replace("http://localhost:7701", ""), body


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def csrf(page):
    m = re.search(r'name="csrf" value="([^"]+)"', page)
    if not m:
        fail("no CSRF token on the page")
    return m.group(1)


print("F5 senaryoları")
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE and s["spec"]["database"] for s in sites):
    fail("f0's site %s is missing (run f0.py first)" % SITE)
agent0 = rss("footops agent")

# A collection of every type the panel shows and edits, written by the app's user.
seed = site_js("""
db.f5.drop();
db.f5.insertMany([
  {_id: ObjectId("6ae1c4a7ec711d712539599d"), ad: "Kurşun kalem", fiyat: 12.5, tam: Double(3), adet: 3, uzun: NumberLong("9007199254740993"),
   ondalik: NumberDecimal("12.50"), tarih: ISODate("2026-10-28T09:47:19.647Z"), etiket: ["a", "b"], ic: {x: 1, y: "z"},
   ikili: BinData(4, "q6urq6urq6urq6urq6urqw=="), yok: null, re: /^ür.n$/i, sifre: "gizli-deger-123"},
  {_id: "musteri-42", ad: "Ayşe", telefon: "0555"},
  {_id: 7, ad: "yedi"}
]);
db.f5.createIndex({ad: 1}, {unique: true});
print("N " + db.f5.countDocuments());
""")
if "N 3" not in seed:
    fail("seed: " + seed)
counts = site_js('print("B "+db.big.countDocuments({})+" "+db.big.countDocuments({b: 7}))').split("B ", 1)[1].split()
big, per_b = int(counts[0]), int(counts[1])
if big < 100000:
    fail("itsite.big has %d documents (f0 seeds 200.000)" % big)
oid = '{"$oid": "6ae1c4a7ec711d712539599d"}'
oid_js = 'ObjectId("6ae1c4a7ec711d712539599d")'
before = stored("f5", oid)

# ---- collections ----
code, ov, ms = timed("GET", "/v1/sites/%s/db" % SITE)
names = {c["name"]: c for c in ov["collections"]}
if code != 200 or names.get("big", {}).get("count") != big or names["f5"]["count"] != 3 or names["f5"]["indexes"] != 2:
    fail("collections: %d %s" % (code, ov))
measure["collections_ms"] = ms
ok("koleksiyonlar: %d koleksiyon, big %d belge %.1f MB (%d ms)" % (len(names), big, names["big"]["size"] / 1e6, ms))

# ---- pages of 200 000 documents ----
code, p, ms = timed("GET", q("/v1/sites/%s/db/find" % SITE, c="big"))
if code != 200 or p["total"] != big or len(p["docs"]) != 20 or not p["more"]:
    fail("big page 1: %d %s" % (code, str(p)[:500]))
measure["page_ms"] = ms
pad = [f["text"] for f in p["docs"][0]["preview"] if f["key"] == "pad"][0]
if not pad.endswith('…"'):
    fail("long string not cut: %s" % pad)
code, p, ms = timed("GET", q("/v1/sites/%s/db/find" % SITE, c="big", page="5000"))
if code != 200 or len(p["docs"]) != 20:
    fail("big page 5000: %d" % code)
measure["page_5000_ms"] = ms
code, p, ms = timed("GET", q("/v1/sites/%s/db/find" % SITE, c="big", filter='{"b": 7}', sort='{"i": -1}'))
if code != 200 or p["total"] != per_b or not p["docs"][0]["id"].startswith('{"$oid": "'):
    fail("filter: %d %s" % (code, str(p)[:300]))
measure["filter_unindexed_ms"] = ms
ok("%d belgelik koleksiyon: 1. sayfa %d ms, 5000. sayfa %d ms, indekssiz filtre + sıralama (%d eşleşme) %d ms"
   % (big, measure["page_ms"], measure["page_5000_ms"], per_b, ms))
for f in ['{"$where": "sleep(100) || true"}', '{"$expr": {"$function": {"body": "function(){return true}", "args": [], "lang": "js"}}}',
          '{"b": {"$foo": 1}}', '{"b": ']:
    code, r, _ = timed("GET", q("/v1/sites/%s/db/find" % SITE, c="big", filter=f))
    if code != 422:
        fail("filter %s: %d %s" % (f, code, r))
if timed("GET", q("/v1/sites/%s/db/find" % SITE, c="system.users"))[0] != 422 or timed("GET", q("/v1/sites/%s/db/find" % SITE, c="yok"))[0] != 404:
    fail("system or missing collection")
ok("$where / $function / bilinmeyen operatör / yarım JSON reddedildi (422); system.* ve olmayan koleksiyon yok")

# ---- one document ----
code, d, ms = timed("GET", q("/v1/sites/%s/db/doc" % SITE, c="f5", id=oid))
if code != 200 or not d["editable"] or '"uzun": {"$numberLong": "9007199254740993"}' not in d["text"] or '"tam": 3.0' not in d["text"]:
    fail("document: %d %s" % (code, d))
measure["doc_ms"] = ms
ok("belge metin olarak (%d ms): int64, double 3.0, decimal, tarih, ikili, regex yazıldığı gibi" % ms)

# ---- an edit through the panel ----
user, pw, secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
# argon2id's 64 MiB goes back to the OS before the login answers (it stayed for minutes).
measure["web_rss_after_login_mb"] = round(rss("footops web") / 1048576, 1)
if measure["web_rss_after_login_mb"] > 40:
    fail("web holds %.1f MB right after a login" % measure["web_rss_after_login_mb"])
ok("girişten hemen sonra web %.1f MB (argon2id'nin 64 MiB'ı geri verildi)" % measure["web_rss_after_login_mb"])
code, _, page = web(q("/sites/%s/db/edit" % SITE, c="f5", id=oid))
if code != 200:
    fail("editor: %d" % code)
tok = csrf(page)
h = re.search(r'name="h" value="([0-9a-f]{64})"', page).group(1)
text = d["text"].replace('"adet": 3', '"adet": 4').replace('"fiyat": 12.5,', '"fiyat": 13.0, "yeni": "eklendi ✓",')
sh("cat > /tmp/f5-text", stdin=text.encode())
form = {"csrf": tok, "c": "f5", "id": oid, "h": h, "text": "</tmp/f5-text", "do": "preview"}
code, _, page = web("/sites/%s/db/edit" % SITE, multipart=form)
if code != 200 or "Değişiklikleri kontrol et" not in page or page.count("note-diff") != 2 or "note-new" not in page:
    fail("preview: %d\n%s" % (code, page[:3000]))
if stored("f5", oid) != before:
    fail("the preview wrote")
form["do"] = "save"
t0 = time.time()
code, loc, _ = web("/sites/%s/db/edit" % SITE, multipart=form)
measure["save_ms"] = round((time.time() - t0) * 1000)
if code != 303 or "/db/doc?" not in loc:
    fail("save: %d %s" % (code, loc))
after = json.loads(canonical("f5", oid_js))
want = {"adet": {"$numberInt": "4"}, "fiyat": {"$numberInt": "13"}, "yeni": "eklendi ✓",
        "uzun": {"$numberLong": "9007199254740993"}, "ondalik": {"$numberDecimal": "12.50"},
        "tarih": {"$date": {"$numberLong": "1793180839647"}}, "ikili": {"$binary": {"base64": "q6urq6urq6urq6urq6urqw==", "subType": "04"}},
        "re": {"$regularExpression": {"pattern": "^ür.n$", "options": "i"}}}
for k, v in want.items():
    if after.get(k) != v:
        fail("after the edit %s = %s, want %s" % (k, after.get(k), v))
t = types("f5", oid_js)
want_types = {"_id": "objectId", "adet": "int", "fiyat": "double", "tam": "double", "uzun": "long", "ondalik": "decimal",
              "tarih": "date", "ikili": "binData", "re": "regex", "yeni": "string", "yok": "null", "ic": "object", "etiket": "array"}
for k, v in want_types.items():
    if t.get(k) != v:
        fail("after the edit %s is %s, want %s (%s)" % (k, t.get(k), v, t))
if list(t)[0] != "_id":
    fail("_id not first")
audit = sh("tail -n 3 /var/lib/footops/audit.jsonl")
if '"action":"db.edit"' not in audit or "alanlar: fiyat, +yeni, adet" not in audit or "Kurşun" in audit or "gizli-deger" in audit:
    fail("audit:\n" + audit)
ok("panelden düzenleme (giriş + CSRF + multipart form → fark → kaydet, %d ms): türler sunucunun $type'ına göre aynen (13.0 double, 4 int, int64, decimal, tarih, ikili, regex); denetimde yalnız alan adları" % measure["save_ms"])

# ---- the app changes the document in between: nothing is written ----
code, d2, _ = timed("GET", q("/v1/sites/%s/db/doc" % SITE, c="f5", id=oid))
site_js('db.f5.updateOne({_id: %s}, {$set: {adet: 99}})' % oid_js)
changed = stored("f5", oid)
code, r, _ = timed("POST", "/v1/sites/%s/db/replace" % SITE, {"collection": "f5", "id": oid, "hash": d2["hash"],
                                                               "text": d2["text"].replace('"ad": "Kurşun kalem"', '"ad": "X"'), "actor": ACTOR})
if code != 409 or stored("f5", oid) != changed:
    fail("conflict: %d %s" % (code, r))
ok("belge arada uygulama tarafından değişti → 409, hiçbir şey yazılmadı")

# ---- undo of the edit: refused while the document differs, then exact ----
_, edits = api("GET", "/v1/sites/%s/db/edits" % SITE)
edit = [e for e in edits if e["op"] == "edit" and e["collection"] == "f5"][0]
code, r, _ = timed("POST", "/v1/sites/%s/db/edits/%s/undo" % (SITE, edit["id"]), {"actor": ACTOR})
if code != 409 or stored("f5", oid) != changed:
    fail("undo after a change: %d %s" % (code, r))
site_js('db.f5.updateOne({_id: %s}, {$set: {adet: 4}})' % oid_js)
code, r, _ = timed("POST", "/v1/sites/%s/db/edits/%s/undo" % (SITE, edit["id"]), {"actor": ACTOR})
if code != 200 or stored("f5", oid) != before:
    fail("undo: %d %s" % (code, r))
ok("düzenlemeyi geri al: belge değişmişken reddedildi; sonra ilk hali bayt bayt geri geldi")

# ---- delete and undo, a string _id ----
code, d3, _ = timed("GET", q("/v1/sites/%s/db/doc" % SITE, c="f5", id='"musteri-42"'))
kept = d3["hash"]
code, rec, _ = timed("POST", "/v1/sites/%s/db/delete" % SITE, {"collection": "f5", "id": '"musteri-42"', "hash": d3["hash"], "actor": ACTOR})
if code != 200 or site_js('print(db.f5.countDocuments({_id: "musteri-42"}))') != "0":
    fail("delete: %d %s" % (code, rec))
code, r, _ = timed("POST", "/v1/sites/%s/db/edits/%s/undo" % (SITE, rec["id"]), {"actor": ACTOR})
if code != 200 or stored("f5", '"musteri-42"') != kept:
    fail("undo delete: %d %s" % (code, r))
if timed("POST", "/v1/sites/%s/db/edits/%s/undo" % (SITE, rec["id"]), {"actor": ACTOR})[0] != 422:
    fail("undo twice")
perm = sh("stat -c '%%a %%U' /var/lib/footops/dbedits /var/lib/footops/dbedits/%s /var/lib/footops/dbedits/%s/%s/before.bson"
          % (SITE, SITE, rec["id"])).split("\n")
if perm[:3] != ["700 root", "700 root", "600 root"]:
    fail("record permissions: %s" % perm)
ok("sil → geri al: belge aynen geri geldi; ikinci geri alma reddedildi; kayıtlar root 0700/0600")

# ---- a job on the site holds changes off ----
_, j = api("POST", "/v1/sites/%s/snapshots" % SITE, {"actor": ACTOR})
code, r, _ = timed("POST", "/v1/sites/%s/db/delete" % SITE, {"collection": "f5", "id": "7", "hash": "x", "actor": ACTOR})
job = wait_job(j["job_id"])
if code != 423 or job.get("error"):
    fail("change during a job: %d %s / %s" % (code, r, job.get("error")))
ok("site üzerinde iş sürerken değişiklik 423 (bekle), iş bitti")

# ---- nothing left running; the agent's memory ----
if tools():
    fail("tool container left: %s" % tools())
if sh("pgrep -x mongosh || true").strip():
    fail("a mongosh is running")
agent1 = rss("footops agent")
measure["agent_rss_mb"] = [round(agent0 / 1048576, 1), round(agent1 / 1048576, 1)]
site_js("db.f5.drop()")
ok("araç container'ı ya da mongosh süreci yok; agent %.1f → %.1f MB" % tuple(measure["agent_rss_mb"]))
print(json.dumps(measure))
