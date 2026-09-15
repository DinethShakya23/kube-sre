(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var md = window.KSMD;
  var KEYSTORE = "kubesre.key";

  var state = { key: "", role: "", session: "", busy: false, abort: null, findTimer: null };

  function safe(fn) { try { return fn(); } catch (e) { return null; } }
  function loadKey() {
    return safe(function () { return sessionStorage.getItem(KEYSTORE) || localStorage.getItem(KEYSTORE) || ""; }) || "";
  }
  function saveKey(k, remember) {
    safe(function () { sessionStorage.removeItem(KEYSTORE); localStorage.removeItem(KEYSTORE); });
    if (!k) return;
    safe(function () { (remember ? localStorage : sessionStorage).setItem(KEYSTORE, k); });
  }
  function newSession() {
    var id = (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : String(Date.now()) + Math.random().toString(16).slice(2);
    state.session = id;
    $("sessionChip").textContent = id.slice(0, 8);
    $("sessionChip").title = id;
  }

  function headers(extra) {
    var h = Object.assign({}, extra || {});
    if (state.key) h["Authorization"] = "Bearer " + state.key;
    return h;
  }

  function errText(t) {
    try {
      var j = JSON.parse(t);
      var m = j.detail || j.error || j.message;
      if (m && typeof m === "object") m = m.message || JSON.stringify(m);
      return m || t;
    } catch (e) { return t; }
  }

  function api(path, opts) {
    opts = opts || {};
    opts.headers = headers(opts.headers);
    return fetch(path, opts).then(function (r) {
      if (r.ok) return r;
      return r.text().then(function (t) {
        var err = new Error(r.status + ": " + errText(t));
        err.status = r.status;
        throw err;
      });
    });
  }
  function getJSON(path) { return api(path).then(function (r) { return r.json(); }); }
  function getText(path) { return api(path).then(function (r) { return r.text(); }); }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function clear(n) { while (n.firstChild) n.removeChild(n.firstChild); }
  function badge(text, kind) { return el("span", "badge " + (kind || ""), text); }

  function banner(msg, bad) {
    var b = $("banner");
    if (!msg) { b.hidden = true; return; }
    b.textContent = msg;
    b.className = "banner" + (bad ? " bad" : "");
    b.hidden = false;
  }

  function fmtTime(t) {
    if (!t) return "";
    var d = new Date(typeof t === "number" ? t * 1000 : t);
    return isNaN(d.getTime()) ? String(t) : d.toLocaleString();
  }

  var loaders = { findings: loadFindings, status: loadStatus, manage: loadManage };
  function show(view) {
    document.querySelectorAll("#nav button").forEach(function (b) { b.classList.toggle("on", b.dataset.view === view); });
    document.querySelectorAll(".view").forEach(function (v) { v.classList.toggle("on", v.id === "view-" + view); });
    safe(function () { history.replaceState(null, "", "#" + view); });
    clearInterval(state.findTimer);
    if (view === "findings") {
      state.findTimer = setInterval(function () { if ($("findAuto").checked) loadFindings(); }, 10000);
    }
    if (loaders[view]) loaders[view]();
  }

  function connect() {
    return api("/v1/auth/whoami").then(function (r) { return r.json(); }).then(function (j) {
      state.role = j.role || "";
      $("dot").className = "dot up";
      $("whoText").textContent = state.role || "connected";
      banner("");
      return true;
    }).catch(function (e) {
      state.role = "";
      $("dot").className = "dot down";
      if (e.status === 401) {
        $("whoText").textContent = "key needed";
        banner("This server needs an API key. Use the API key link to connect.", true);
      } else {
        $("whoText").textContent = "unreachable";
        banner("Cannot reach the server: " + e.message, true);
      }
      return false;
    });
  }
  function canWrite() { return ["operator", "admin", "superadmin"].indexOf(state.role) >= 0; }
  function isAdmin() { return state.role === "admin" || state.role === "superadmin"; }

  function addMsg(kind, text) {
    var m = el("div", "msg " + kind);
    if (text != null) m.textContent = text;
    $("thread").appendChild(m);
    $("thread").scrollTop = $("thread").scrollHeight;
    return m;
  }

  function setBusy(b) {
    state.busy = b;
    $("sendBtn").disabled = b;
    $("stopBtn").hidden = !b;
  }

  function send(text) {
    if (!text.trim() || state.busy) return;
    addMsg("user", text);
    $("approval").hidden = true;
    var bot = addMsg("bot");
    var acts = el("details", "activity");
    var sum = el("summary", null, "Working");
    var list = el("ul");
    acts.appendChild(sum);
    acts.appendChild(list);
    var body = el("div", "cursor");
    bot.appendChild(acts);
    bot.appendChild(body);
    var answer = "", steps = 0, failed = false;

    function step(txt) {
      steps++;
      list.appendChild(el("li", null, txt));
      sum.textContent = "Working (" + steps + " step" + (steps === 1 ? "" : "s") + ")";
    }
    function paint() {
      body.innerHTML = md.render(answer);
      $("thread").scrollTop = $("thread").scrollHeight;
    }
    function frame(data) {
      if (data === "[DONE]") return;
      var j;
      try { j = JSON.parse(data); } catch (e) { return; }
      if (j.object === "stream.start") return;
      var ev = j.ki_event;
      if (ev) {
        if (ev.type === "status") step(ev.message || ev.phase || "working");
        else if (ev.type === "tool_call") step("call " + (ev.tool || "tool") + (ev.message ? ": " + ev.message : ""));
        else if (ev.type === "tool_result") step("result from " + (ev.tool || "tool"));
        else if (ev.type === "plan") step("plan with " + (ev.steps || []).length + " steps");
        else if (ev.type === "usage" && ev.total_tokens) bot.appendChild(el("div", "usage", ev.total_tokens + " tokens"));
      }
      if (j.hitl_required) {
        $("approvalText").textContent = (j.risk_level ? "[" + j.risk_level + "] " : "") + (j.human_summary || "An action needs your approval.");
        $("approval").hidden = false;
      }
      var d = j.choices && j.choices[0] && j.choices[0].delta && j.choices[0].delta.content;
      if (d) { answer += d; paint(); }
    }

    state.abort = new AbortController();
    setBusy(true);
    fetch("/v1/chat/completions", {
      method: "POST",
      signal: state.abort.signal,
      headers: headers({ "Content-Type": "application/json", "X-Session-ID": state.session }),
      body: JSON.stringify({ model: "kube-sre", stream: true, user: "web", messages: [{ role: "user", content: text }] })
    }).then(function (r) {
      if (!r.ok) {
        return r.text().then(function (t) {
          var e = new Error(r.status + ": " + errText(t));
          e.status = r.status;
          throw e;
        });
      }
      var reader = r.body.getReader(), dec = new TextDecoder(), buf = "";
      function pump() {
        return reader.read().then(function (res) {
          if (res.done) return;
          buf += dec.decode(res.value, { stream: true });
          var parts = buf.split("\n\n");
          buf = parts.pop();
          parts.forEach(function (p) {
            p.split("\n").forEach(function (l) { if (l.indexOf("data:") === 0) frame(l.slice(5).trim()); });
          });
          return pump();
        });
      }
      return pump();
    }).catch(function (e) {
      if (e.name === "AbortError") { answer += "\n\n_Stopped._"; return; }
      failed = true;
      answer = (e.status === 401 ? "The server rejected the key. " : "") + "Request failed: " + e.message;
    }).then(function () {
      body.classList.remove("cursor");
      if (failed) bot.classList.add("err");
      if (!answer) answer = "_No answer came back._";
      paint();
      setBusy(false);
      state.abort = null;
    });
  }

  function decide(word) {
    $("approval").hidden = true;
    send(word);
  }

  function loadFindings() {
    getJSON("/v1/findings?limit=100").then(function (j) {
      var meta = $("findMeta");
      clear(meta);
      var c = el("div", "card");
      c.appendChild(el("h3", null, "Sensorium"));
      var sb = el("div", "big");
      sb.appendChild(badge(j.sensorium || "unknown", j.sensorium === "disabled" || !j.sensorium ? "warn" : "ok"));
      c.appendChild(sb);
      if (j.sensorium_reason) c.appendChild(el("div", "small", j.sensorium_reason));
      meta.appendChild(c);
      var q = j.queue || {};
      var c2 = el("div", "card");
      c2.appendChild(el("h3", null, "Queue"));
      c2.appendChild(el("div", "big", "shed " + (q.shed_total || 0)));
      c2.appendChild(el("div", "small", "high water " + (q.high_water || 0) + " of " + (q.maxsize || 0)));
      meta.appendChild(c2);
      if (j.predictive_error) {
        var c3 = el("div", "card");
        c3.appendChild(el("h3", null, "Predictive"));
        c3.appendChild(el("div", "small", j.predictive_error));
        meta.appendChild(c3);
      }
      var tb = $("findTable").tBodies[0];
      clear(tb);
      var rows = j.findings || [];
      $("findEmpty").hidden = rows.length > 0;
      $("findEmpty").textContent = j.sensorium === "disabled" ? "Perception is off, so no findings are collected." : "No findings. Nothing has fired.";
      rows.forEach(function (f) {
        var tr = el("tr");
        tr.appendChild(el("td", null, fmtTime(f.fired_at)));
        tr.appendChild(el("td", "mono", f.playbook || ""));
        var sv = el("td");
        sv.appendChild(badge(f.severity || "", f.severity === "predicted" ? "warn" : "bad"));
        if (f.eta_minutes != null) sv.appendChild(el("div", "sub", "in about " + Math.round(f.eta_minutes) + " min"));
        tr.appendChild(sv);
        var obj = (f.namespace ? f.namespace + "/" : "") + (f.object || "");
        tr.appendChild(el("td", "mono", obj));
        tr.appendChild(el("td", null, f.evidence || ""));
        var act = el("td");
        var b = el("button", "ghost", "Investigate");
        b.type = "button";
        b.addEventListener("click", function () {
          show("chat");
          $("input").value = "Investigate " + (f.playbook || "this finding") + " on " + obj + ". Evidence: " + (f.evidence || "");
          $("input").focus();
        });
        act.appendChild(b);
        tr.appendChild(act);
        tb.appendChild(tr);
      });
    }).catch(function (e) { banner("Findings: " + e.message, true); });
  }

  function loadDigest() {
    var box = $("digestBody");
    box.textContent = "Loading...";
    getText("/v1/digest?format=markdown&hours=" + encodeURIComponent($("digestHours").value)).then(function (t) {
      box.innerHTML = md.render(t);
    }).catch(function (e) { box.textContent = "Could not load the digest. " + e.message; });
  }

  function openReport(id) {
    id = (id || "").trim();
    if (!id) return;
    show("reports");
    $("reportId").value = id;
    var body = $("reportBody"), v = $("reportVerdict");
    clear(v);
    body.textContent = "Loading...";
    $("replayBox").hidden = true;
    var enc = encodeURIComponent(id);
    getJSON("/v1/episodes/" + enc + "/postmortem?format=markdown").then(function (j) {
      body.innerHTML = md.render(j.markdown || j.postmortem || "");
      var verdict = el("div", "verdict");
      if (j.chain_valid === false) {
        verdict.classList.add("bad");
        verdict.textContent = "The recorded chain does not verify. Treat this timeline as unreliable.";
      } else if (j.chain_verified === false) {
        verdict.classList.add("warn");
        verdict.textContent = "The chain could not be verified. That does not mean it was altered.";
      } else {
        verdict.classList.add("ok");
        verdict.textContent = "The recorded chain verifies.";
      }
      var extra = [];
      if (j.events_lost) extra.push(j.events_lost + " events lost");
      if (j.gaps && j.gaps.length) extra.push(j.gaps.length + " gaps");
      if (j.enrichment_failed) extra.push("enrichment failed");
      if (extra.length) verdict.textContent += " (" + extra.join(", ") + ")";
      v.appendChild(verdict);
      loadReplay(enc);
    }).catch(function (e) {
      body.textContent = e.status === 404 ? "No record for that id." : "Could not load the postmortem. " + e.message;
    });
  }

  function loadReplay(enc) {
    getText("/v1/episodes/" + enc + "/replay").then(function (t) {
      var tb = $("replayTable").tBodies[0];
      clear(tb);
      var n = 0;
      t.split("\n").forEach(function (l) {
        if (l.indexOf("data:") !== 0) return;
        var d = l.slice(5).trim();
        if (!d || d === "[DONE]") return;
        var j;
        try { j = JSON.parse(d); } catch (e) { return; }
        n++;
        var tr = el("tr");
        tr.appendChild(el("td", "mono", String(j.seq != null ? j.seq : n)));
        tr.appendChild(el("td", "mono", j.type || j.event_type || ""));
        var s = JSON.stringify(j.payload != null ? j.payload : j);
        tr.appendChild(el("td", "mono", s.length > 400 ? s.slice(0, 400) + "..." : s));
        tb.appendChild(tr);
      });
      $("replayBox").hidden = n === 0;
    }).catch(function () { $("replayBox").hidden = true; });
  }

  function kv(pairs) {
    var dl = el("dl", "kv");
    pairs.forEach(function (p) {
      dl.appendChild(el("dt", null, p[0]));
      dl.appendChild(el("dd", null, p[1] == null || p[1] === "" ? "-" : String(p[1])));
    });
    return dl;
  }
  function card(title, big, kind, node) {
    var c = el("div", "card");
    c.appendChild(el("h3", null, title));
    if (big != null) {
      var b = el("div", "big");
      b.appendChild(badge(big, kind));
      c.appendChild(b);
    }
    if (node) c.appendChild(node);
    return c;
  }

  function loadStatus() {
    Promise.all([getJSON("/healthz"), getJSON("/v1/v5/status").catch(function () { return null; })]).then(function (r) {
      var h = r[0], s = r[1], box = $("statusBody");
      clear(box);
      var grid = el("div", "cards");
      grid.appendChild(card("Server", h.status || "unknown", h.status === "ok" ? "ok" : "warn", kv([["version", h.version], ["arm", h.arm]])));
      var mem = h.memory || {};
      grid.appendChild(card("Memory", mem.state || "off", mem.state === "ok" || mem.state === "enabled" ? "ok" : "warn", mem.reason ? el("div", "small", mem.reason) : null));
      var ld = h.leader || {};
      grid.appendChild(card("Leader", ld.is_leader ? "leader" : "follower", "", el("div", "small", ld.reason || "")));
      if (s) {
        grid.appendChild(card("Kill switch", s.kill_switch_engaged ? "engaged" : "off", s.kill_switch_engaged ? "bad" : "ok"));
        grid.appendChild(card("Change freeze", s.change_freeze ? "active" : "off", s.change_freeze ? "warn" : "ok"));
        grid.appendChild(card("Spend cap", s.spend_cap_usd ? "$" + s.spend_cap_usd : "none", ""));
        var ap = s.autonomy_promotion;
        if (ap) {
          var txt = ap.reason || JSON.stringify(ap);
          grid.appendChild(card("Autonomy promotion", ap.state || ap.status || "reported", "", el("div", "small", txt.slice(0, 200))));
        }
      }
      box.appendChild(grid);
      var flags = (s && s.active_flags) || h.experimental_flags || [];
      var un = (s && s.set_but_unwired_flags) || h.set_but_unwired_flags || [];
      var p = el("div", "panel");
      p.appendChild(el("h2", null, "Experimental flags"));
      p.appendChild(kv([["active", flags.join(", ")], ["set but not wired", un.join(", ")]]));
      if (s && s.unenforceable_guard_config && s.unenforceable_guard_config.length) {
        p.appendChild(el("p", "err", "Guard settings that protect nothing: " + s.unenforceable_guard_config.join("; ")));
      }
      box.appendChild(p);
    }).catch(function (e) { $("statusBody").textContent = "Could not read status. " + e.message; });
  }

  function loadManage() {
    var writer = canWrite(), admin = isAdmin();
    $("prefForm").hidden = !writer;
    $("detForm").hidden = !admin;

    getJSON("/v1/detectors").then(function (j) {
      var box = $("detList");
      clear(box);
      var rows = j.detectors || [];
      if (!rows.length) box.appendChild(el("p", "empty", "No authored detectors."));
      rows.forEach(function (d) {
        var it = el("div", "item");
        var left = el("div");
        left.appendChild(el("div", "name", d.name));
        var m = el("div", "meta");
        m.appendChild(badge(d.status, d.status === "active" ? "ok" : "warn"));
        m.appendChild(document.createTextNode(" " + (d.source || "")));
        left.appendChild(m);
        it.appendChild(left);
        if (admin) {
          var acts = el("div", "acts");
          var verb = d.status === "active" ? "demote" : "promote";
          var b = el("button", "ghost", verb === "demote" ? "Demote" : "Promote");
          b.type = "button";
          b.addEventListener("click", function () {
            if (!confirm(verb + " detector " + d.name + "?")) return;
            api("/v1/detectors/" + encodeURIComponent(d.name) + "/" + verb, { method: "POST" }).then(loadManage).catch(function (e) { banner(e.message, true); });
          });
          acts.appendChild(b);
          it.appendChild(acts);
        }
        box.appendChild(it);
      });
    }).catch(function (e) {
      $("detList").textContent = e.status === 404 || e.status === 403 ? "Detectors are not enabled for this key." : "Could not list detectors. " + e.message;
    });

    getJSON("/v1/preferences").then(function (j) {
      var box = $("prefList");
      clear(box);
      var rows = j.preferences || [];
      if (!rows.length) box.appendChild(el("p", "empty", "Nothing remembered yet."));
      rows.forEach(function (p) {
        var it = el("div", "item");
        var left = el("div");
        left.appendChild(el("div", "name", p.key + " = " + p.value));
        left.appendChild(el("div", "meta", p.source + (p.occurrence_count ? ", seen " + p.occurrence_count + "x" : "")));
        it.appendChild(left);
        if (writer) {
          var acts = el("div", "acts");
          var b = el("button", "ghost", "Forget");
          b.type = "button";
          b.addEventListener("click", function () {
            api("/v1/preferences/" + encodeURIComponent(p.key), { method: "DELETE" }).then(loadManage).catch(function (e) { banner(e.message, true); });
          });
          acts.appendChild(b);
          it.appendChild(acts);
        }
        box.appendChild(it);
      });
    }).catch(function (e) {
      $("prefList").textContent = e.status === 404 || e.status === 503 ? "Memory is not enabled, so preferences are unavailable." : "Could not list preferences. " + e.message;
    });
  }

  function currentView() {
    var v = (location.hash || "").replace("#", "");
    return $("view-" + v) ? v : "chat";
  }

  function wire() {
    document.querySelectorAll("#nav button").forEach(function (b) {
      b.addEventListener("click", function () { show(b.dataset.view); });
    });
    $("composer").addEventListener("submit", function (e) {
      e.preventDefault();
      var t = $("input").value;
      $("input").value = "";
      send(t);
    });
    $("input").addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("composer").requestSubmit(); }
    });
    $("stopBtn").addEventListener("click", function () { if (state.abort) state.abort.abort(); });
    $("approveBtn").addEventListener("click", function () { decide("yes"); });
    $("denyBtn").addEventListener("click", function () { decide("no"); });
    $("newChat").addEventListener("click", function () {
      if (state.abort) state.abort.abort();
      clear($("thread"));
      $("approval").hidden = true;
      newSession();
    });
    $("reportBtn").addEventListener("click", function () { openReport(state.session); });
    $("findRefresh").addEventListener("click", loadFindings);
    $("digestLoad").addEventListener("click", loadDigest);
    $("statusRefresh").addEventListener("click", loadStatus);
    $("reportForm").addEventListener("submit", function (e) { e.preventDefault(); openReport($("reportId").value); });

    $("detForm").addEventListener("submit", function (e) {
      e.preventDefault();
      var desc = $("detDesc").value.trim();
      if (!desc) return;
      $("detCreate").disabled = true;
      api("/v1/detectors", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ description: desc, name: $("detName").value.trim() })
      }).then(function (r) { return r.json(); }).then(function (j) {
        if (j.staged === false) {
          banner("Could not compile that: " + ((j.errors || []).join("; ") || "no valid detector"), true);
        } else {
          banner("");
          $("detDesc").value = "";
          $("detName").value = "";
        }
        loadManage();
      }).catch(function (err) { banner(err.message, true); }).then(function () { $("detCreate").disabled = false; });
    });

    $("prefForm").addEventListener("submit", function (e) {
      e.preventDefault();
      var k = $("prefKey").value.trim(), v = $("prefVal").value;
      if (!k) return;
      api("/v1/preferences", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key: k, value: v })
      }).then(function () {
        $("prefKey").value = "";
        $("prefVal").value = "";
        loadManage();
      }).catch(function (err) { banner(err.message, true); });
    });

    var dlg = $("keyDialog");
    $("keyBtn").addEventListener("click", function () {
      $("keyInput").value = state.key;
      $("keyErr").hidden = true;
      dlg.showModal();
    });
    $("keyCancel").addEventListener("click", function () { dlg.close(); });
    $("keyForm").addEventListener("submit", function (e) {
      e.preventDefault();
      state.key = $("keyInput").value.trim();
      connect().then(function (ok) {
        if (ok) {
          saveKey(state.key, $("keyRemember").checked);
          dlg.close();
          show(currentView());
        } else {
          $("keyErr").textContent = "That key was not accepted.";
          $("keyErr").hidden = false;
        }
      });
    });
  }

  wire();
  state.key = loadKey();
  newSession();
  connect().then(function () { show(currentView()); });
})();
