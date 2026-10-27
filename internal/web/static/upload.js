// Chunked, resumable uploads (see agent/uploads.go). The file is sent in chunks the
// server sized (8 MB: one chunk fits Cloudflare's 100-second limit even on a slow home
// connection), each with its SHA-256, one after another. Choosing the same file again
// continues with the chunks the server still lacks. Without JavaScript the same form
// posts the file in one request instead (up to 90 MB).
(function () {
  "use strict";
  var form = document.querySelector("form[data-upload]");
  if (!form || !window.fetch || !window.crypto || !crypto.subtle || !window.Blob || !Blob.prototype.arrayBuffer) return;
  var site = form.getAttribute("data-site");
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

  // The upload of a file is remembered per browser, so choosing it again resumes.
  function key(f) { return "footops-upload:" + site + ":" + f.name + ":" + f.size + ":" + f.lastModified; }
  function remember(k, v) { try { if (v) { localStorage.setItem(k, v); } else { localStorage.removeItem(k); } } catch (e) { /* private mode */ } }
  function recall(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }

  function begin(file) {
    var id = recall(key(file));
    var resume = id ? api("GET", "/uploads/" + id).then(function (st) {
      return st.site === site && st.size === file.size && !st.finished ? st : null;
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
      status.textContent = "Parça " + (n + 1) + " gönderilemedi, tekrar deneniyor (" + (tries + 1) + "/3)…";
      return wait(2000 * (tries + 1)).then(function () { return send(file, st, n, tries + 1); });
    });
  }

  function show(done, total, rate) {
    bar.value = total ? Math.floor(done / total * 1000) : 0;
    var text = human(done) + " / " + human(total) + " · %" + (total ? Math.floor(done / total * 100) : 0);
    if (rate > 0 && done < total) text += " · " + human(rate) + "/sn · kalan ~" + duration((total - done) / rate);
    status.textContent = text;
  }

  function busy(on) {
    submit.disabled = on;
    input.disabled = on;
    stop.hidden = !on;
  }

  function run(file) {
    stopped = false;
    ctrl = window.AbortController ? new AbortController() : null;
    box.hidden = false;
    box.classList.remove("failed");
    busy(true);
    status.textContent = "Hazırlanıyor…";
    var started = Date.now();
    var sent = 0;
    begin(file).then(function (st) {
      remember(key(file), st.id);
      var missing = st.missing.slice();
      var done = file.size;
      missing.forEach(function (n) { done -= Math.min(st.chunk, file.size - n * st.chunk); });
      show(done, file.size, 0);
      var i = 0;
      function next() {
        if (stopped) throw new Error("Durduruldu. Aynı dosyayı tekrar seçince kaldığı yerden devam eder.");
        if (i >= missing.length) {
          status.textContent = "Yüklendi; kontrol başlatılıyor…";
          return api("POST", "/uploads/" + st.id + "/finish", {});
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
      location.assign(res.next);
    }).catch(function (err) {
      box.classList.add("failed");
      status.textContent = stopped ? "Durduruldu. Aynı dosyayı tekrar seçince kaldığı yerden devam eder." : (err && err.message) || "Yükleme durdu.";
      busy(false);
    });
  }

  form.addEventListener("submit", function (ev) {
    var file = input.files && input.files[0];
    if (!file) return;
    ev.preventDefault();
    if (max && file.size > max) {
      box.hidden = false;
      box.classList.add("failed");
      status.textContent = "Dosya çok büyük: " + human(file.size) + " (sınır " + human(max) + ").";
      return;
    }
    run(file);
  });
  stop.addEventListener("click", function () {
    stopped = true;
    if (ctrl) ctrl.abort();
  });
})();
