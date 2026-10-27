// Chunked, resumable uploads (see agent/uploads.go). A file is sent in chunks the server
// sized (8 MB: one chunk fits Cloudflare's 100-second limit even on a slow home
// connection), each with its SHA-256, one after another. Choosing the same file again
// continues with the chunks the server still lacks. Without JavaScript the same form
// posts the file in one request instead (up to 90 MB).
//
// Two pages use it: the import page (one archive, then its check) and the files page
// (data-mode="files": several files, each put into the folder once it has arrived).
(function () {
  "use strict";
  var form = document.querySelector("form[data-upload]");
  if (!form || !window.fetch || !window.crypto || !crypto.subtle || !window.Blob || !Blob.prototype.arrayBuffer) return;
  var site = form.getAttribute("data-site");
  var files = form.getAttribute("data-mode") === "files";
  var place = form.getAttribute("data-place");
  var max = parseInt(form.getAttribute("data-max"), 10) || 0;
  var csrf = form.querySelector("input[name=csrf]").value;
  var input = form.querySelector("input[type=file]");
  var box = form.querySelector("[data-upload-progress]");
  var bar = box.querySelector("progress");
  var status = box.querySelector("[data-upload-status]");
  var submit = form.querySelector("button[type=submit]");
  var stop = form.querySelector("[data-upload-stop]");
  var ctrl = null;
  var stopped = false;
  var label = ""; // "2/5 · foto.jpg · " while several files go

  function api(method, url, body, headers) {
    var h = { "X-CSRF-Token": csrf };
    Object.keys(headers || {}).forEach(function (k) { h[k] = headers[k]; });
    if (body && !(body instanceof ArrayBuffer)) {
      h["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    }
    return fetch(url, { method: method, headers: h, body: body, credentials: "same-origin", cache: "no-store", signal: ctrl && ctrl.signal })
      .then(function (res) {
        if (res.redirected && new URL(res.url).pathname === "/login") {
          throw new Error("Oturum kapandı. Tekrar giriş yap ve aynı dosyayı seç: kaldığı yerden devam eder.");
        }
        return res.json().catch(function () { return {}; }).then(function (j) {
          if (!res.ok) {
            var e = new Error(j.error || "Sunucu " + res.status + " döndü");
            e.status = res.status;
            throw e;
          }
          return j;
        });
      });
  }

  function hex(buf) {
    return Array.prototype.map.call(new Uint8Array(buf), function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
  }

  function human(n) {
    if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
    if (n >= 1 << 10) return (n / (1 << 10)).toFixed(0) + " KB";
    return n + " B";
  }

  function duration(s) {
    if (s < 60) return Math.ceil(s) + " sn";
    if (s < 3600) return Math.round(s / 60) + " dk";
    return (s / 3600).toFixed(1) + " sa";
  }

  function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  function say(text) { status.textContent = label + text; }

  // The upload of a file is remembered per browser, so choosing it again resumes.
  function key(f) { return "footops-upload:" + site + ":" + f.name + ":" + f.size + ":" + f.lastModified; }
  function remember(k, v) { try { if (v) { localStorage.setItem(k, v); } else { localStorage.removeItem(k); } } catch (e) { /* private mode */ } }
  function recall(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }

  // An unfinished upload resumes. On the files page a finished one that was not put
  // into the folder yet (the site was busy, the connection dropped) is put there without
  // being sent again.
  function begin(file) {
    var id = recall(key(file));
    var resume = id ? api("GET", "/uploads/" + id).then(function (st) {
      return st.site === site && st.size === file.size && (!st.finished || files) ? st : null;
    }).catch(function () { return null; }) : Promise.resolve(null);
    return resume.then(function (st) {
      return st || api("POST", "/sites/" + encodeURIComponent(site) + "/uploads", { name: file.name, size: file.size });
    });
  }

  // A chunk is retried after network errors, server errors and a damaged arrival (422).
  function send(file, st, n, tries) {
    var from = n * st.chunk;
    return file.slice(from, Math.min(file.size, from + st.chunk)).arrayBuffer().then(function (buf) {
      return crypto.subtle.digest("SHA-256", buf).then(function (sum) {
        return api("PUT", "/uploads/" + st.id + "/" + n, buf, { "X-Chunk-Sha256": hex(sum) });
      });
    }).catch(function (err) {
      var again = !stopped && tries < 3 && (!err.status || err.status >= 500 || err.status === 422);
      if (!again) throw err;
      say("Parça " + (n + 1) + " gönderilemedi, tekrar deneniyor (" + (tries + 1) + "/3)…");
      return wait(2000 * (tries + 1)).then(function () { return send(file, st, n, tries + 1); });
    });
  }

  // finish closes an upload: the import page checks it; the files page puts it into the
  // folder, waiting while a job works on the site (423) for up to five minutes.
  function finish(st, tries) {
    if (!files) return api("POST", "/uploads/" + st.id + "/finish", {});
    var over = form.querySelector("input[name=overwrite]");
    var body = { upload: st.id, dir: form.querySelector("input[name=dir]").value, overwrite: !!(over && over.checked) };
    return api("POST", place, body).catch(function (err) {
      if (err.status !== 423 || stopped || tries >= 60) throw err;
      say("Sitede başka bir işlem sürüyor; bitmesi bekleniyor…");
      return wait(5000).then(function () { return finish(st, tries + 1); });
    });
  }

  function show(done, total, rate) {
    bar.value = total ? Math.floor(done / total * 1000) : 0;
    var text = human(done) + " / " + human(total) + " · %" + (total ? Math.floor(done / total * 100) : 0);
    if (rate > 0 && done < total) text += " · " + human(rate) + "/sn · kalan ~" + duration((total - done) / rate);
    say(text);
  }

  function busy(on) {
    submit.disabled = on;
    input.disabled = on;
    stop.hidden = !on;
  }

  // one sends a file and finishes it.
  function one(file) {
    var started = Date.now();
    var sent = 0;
    say("Hazırlanıyor…");
    return begin(file).then(function (st) {
      remember(key(file), st.id);
      var missing = st.finished ? [] : st.missing.slice();
      var done = file.size;
      missing.forEach(function (n) { done -= Math.min(st.chunk, file.size - n * st.chunk); });
      show(done, file.size, 0);
      var i = 0;
      function next() {
        if (stopped) throw new Error("Durduruldu. Aynı dosyayı tekrar seçince kaldığı yerden devam eder.");
        if (i >= missing.length) {
          say(files ? "Yüklendi; klasöre konuyor…" : "Yüklendi; kontrol başlatılıyor…");
          return finish(st, 0);
        }
        var n = missing[i++];
        var len = Math.min(st.chunk, file.size - n * st.chunk);
        return send(file, st, n, 0).then(function () {
          done += len;
          sent += len;
          show(done, file.size, sent / Math.max(1, (Date.now() - started) / 1000));
          return next();
        });
      }
      return next();
    }).then(function (res) {
      remember(key(file), null);
      return res;
    });
  }

  function run(list) {
    stopped = false;
    ctrl = window.AbortController ? new AbortController() : null;
    box.hidden = false;
    box.classList.remove("failed");
    busy(true);
    var i = 0;
    var last = null;
    function next() {
      if (i >= list.length) return Promise.resolve(last);
      var file = list[i++];
      label = list.length > 1 ? i + "/" + list.length + " · " + file.name + " · " : "";
      return one(file).then(function (res) {
        last = res;
        return next();
      });
    }
    next().then(function (res) {
      label = "";
      // The files page shows what arrived (the server keeps the messages); the import
      // page goes on to the check.
      location.assign(files ? location.pathname + location.search : res.next);
    }).catch(function (err) {
      box.classList.add("failed");
      say(stopped ? "Durduruldu. Aynı dosyayı tekrar seçince kaldığı yerden devam eder." : (err && err.message) || "Yükleme durdu.");
      if (files && i > 1) status.textContent += " Önceki dosyalar yüklendi; sayfayı yenileyince görünür.";
      busy(false);
    });
  }

  form.addEventListener("submit", function (ev) {
    var list = Array.prototype.slice.call(input.files || []);
    if (!list.length) return;
    ev.preventDefault();
    var big = list.filter(function (f) { return max && f.size > max; });
    if (big.length) {
      box.hidden = false;
      box.classList.add("failed");
      label = "";
      say("Dosya çok büyük: " + big[0].name + ", " + human(big[0].size) + " (sınır " + human(max) + ").");
      return;
    }
    run(files ? list : list.slice(0, 1));
  });
  stop.addEventListener("click", function () {
    stopped = true;
    if (ctrl) ctrl.abort();
  });
})();
