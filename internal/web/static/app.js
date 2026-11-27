(function () {
  "use strict";

  // Ask before destructive form submissions: <form data-confirm="..."> or <button data-confirm="...">.
  document.addEventListener("submit", function (e) {
    var el = e.submitter && e.submitter.getAttribute("data-confirm") ? e.submitter : e.target;
    var msg = el.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // Mirrors statusOf() in server.go: label, tone, pulse.
  var STATUS = {
    queued: ["Queued", "neutral", false],
    running: ["Running", "info", true],
    success: ["Succeeded", "ok", false],
    failed: ["Failed", "err", false]
  };
  function setStatus(el, s) {
    var m = STATUS[s] || [s, "neutral", false];
    el.className = "status tone-" + m[1] + (m[2] ? " pulse" : "");
    var label = el.querySelector(".label");
    if (label) label.textContent = m[0];
  }

  // Close the account menu when clicking elsewhere.
  document.addEventListener("click", function (e) {
    document.querySelectorAll("details.menu[open]").forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute("open");
    });
  });

  // Recovery codes are shown in the response to turning two-factor login on;
  // point the address bar back at Settings so a reload doesn't resubmit.
  if (location.pathname === "/settings/2fa/enable") history.replaceState(null, "", "/settings#security");

  // Database import: upload the file in pieces (each fits under the panel
  // proxy's request size limit), then submit the form with the upload's id.
  var imp = document.getElementById("import-form");
  if (imp) {
    var fileInput = imp.querySelector('input[type="file"]');
    var uploadField = imp.querySelector('input[name="upload"]');
    var progress = imp.querySelector(".upload-progress");
    var bar = imp.querySelector(".upload-progress .bar span");
    var status = imp.querySelector(".upload-status");
    var errBox = imp.querySelector(".upload-error");
    var pick = function (v) {
      var r = imp.querySelector('input[name="from"][value="' + v + '"]');
      if (r && !r.disabled) r.checked = true;
    };
    fileInput.addEventListener("change", function () { pick("upload"); uploadField.value = ""; });
    var serverSel = imp.querySelector('select[name="file"]');
    if (serverSel) serverSel.addEventListener("change", function () { pick("server"); });
    var fmt = function (n) {
      var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
      while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
      return (i ? n.toFixed(1) : n) + " " + u[i];
    };
    var showErr = function (msg) { errBox.textContent = msg; errBox.hidden = false; };
    var sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };
    var send = async function (f) {
      var CHUNK = 3 * 1024 * 1024, id = "", off = 0;
      progress.hidden = false;
      while (off < f.size) {
        var url = "/backups/import/upload?offset=" + off + (id ? "&id=" + id : "&total=" + f.size);
        var res = null;
        for (var attempt = 0; attempt < 4 && !res; attempt++) {
          if (attempt) { status.textContent = "Connection hiccup; retrying…"; await sleep(2000 * attempt); }
          try {
            res = await fetch(url, {
              method: "POST", credentials: "same-origin", body: f.slice(off, off + CHUNK),
              headers: { "X-CSRF-Token": imp.dataset.csrf, "Content-Type": "application/octet-stream" }
            });
          } catch (e) { res = null; }
        }
        if (!res) throw new Error("The upload failed: the server can't be reached.");
        var j = await res.json().catch(function () { return {}; });
        if (!res.ok || !j.id) throw new Error(j.error || "The upload failed (HTTP " + res.status + "). Reload the page and try again.");
        id = j.id; off = j.size;
        bar.style.width = (100 * off / f.size).toFixed(1) + "%";
        status.textContent = "Uploaded " + fmt(off) + " of " + fmt(f.size);
      }
      return id;
    };
    imp.addEventListener("submit", function (e) {
      if (e.defaultPrevented) return;
      errBox.hidden = true;
      var from = imp.querySelector('input[name="from"]:checked').value;
      if (from !== "upload" || uploadField.value) return;
      e.preventDefault();
      var f = fileInput.files[0];
      if (!f) { showErr("Choose a file to upload."); return; }
      if (f.size === 0) { showErr("That file is empty."); return; }
      var btn = imp.querySelector("button");
      btn.disabled = true;
      send(f).then(function (id) {
        uploadField.value = id;
        status.textContent = "Uploaded. Starting the import…";
        fileInput.disabled = true; // the file is on the server now; don't send it again
        imp.submit();
      }).catch(function (err) {
        btn.disabled = false;
        progress.hidden = true;
        showErr(err.message);
      });
    });
  }

  // Live job log.
  var log = document.getElementById("job-log");
  if (log) {
    var id = log.getAttribute("data-job");
    var offset = 0;
    var status = document.getElementById("job-status");
    var duration = document.getElementById("job-duration");
    var poll = function () {
      fetch("/jobs/" + id + "/log?offset=" + offset, { credentials: "same-origin" })
        .then(function (r) {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        })
        .then(function (d) {
          var stick = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
          if (d.data) log.appendChild(document.createTextNode(d.data));
          offset = d.offset;
          if (stick) log.scrollTop = log.scrollHeight;
          setStatus(status, d.status);
          if (d.duration) duration.textContent = d.duration;
          if (d.link) {
            var next = document.getElementById("job-next");
            var a = document.getElementById("job-next-link");
            a.href = d.link;
            a.textContent = "Open " + d.link.replace(/\/settings$/, "");
            next.hidden = false;
          }
          if (!d.done || d.more) setTimeout(poll, d.more ? 50 : 1000);
        })
        .catch(function () { setTimeout(poll, 3000); });
    };
    poll();
  }

  // Commands page: presets / history fill the box; risky commands confirm;
  // Cmd/Ctrl+Enter runs.
  var cmdBox = document.getElementById("command");
  if (cmdBox) {
    document.querySelectorAll("[data-command]").forEach(function (b) {
      b.addEventListener("click", function () {
        cmdBox.value = b.getAttribute("data-command");
        cmdBox.focus();
        window.scrollTo({ top: 0, behavior: "smooth" });
      });
    });
    var risky = /migrate:(fresh|reset|refresh|rollback)|db:wipe|\brm\s+-[a-z]*r|drop\s+(table|database)/i;
    document.getElementById("command-form").addEventListener("submit", function (e) {
      if (risky.test(cmdBox.value) && !window.confirm("This command can delete data:\n\n" + cmdBox.value + "\n\nRun it?")) {
        e.preventDefault();
      }
    });
    cmdBox.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        document.getElementById("command-form").requestSubmit();
      }
    });
  }

  // New site: the framework picks the project type; show only its fields and
  // fill in its defaults (unless the user already typed something else).
  var fw = document.getElementById("framework");
  if (fw) {
    var lastDefaults = { start: "", output: "" };
    var applyFramework = function (fill) {
      var opt = fw.options[fw.selectedIndex];
      var kind = opt.getAttribute("data-kind");
      document.querySelectorAll("[data-kinds]").forEach(function (el) {
        el.hidden = el.getAttribute("data-kinds").split(" ").indexOf(kind) === -1;
      });
      if (!fill) return;
      // A database is the usual choice for Laravel, not for JS or Python apps,
      // unless the user picked one.
      var db = document.getElementById("database");
      if (db && !db.dataset.touched && db.options.length > 1) db.value = kind === "laravel" ? db.options[1].value : "";
      [["start_command", "start", "data-start"], ["output_dir", "output", "data-output"]].forEach(function (f) {
        var input = document.getElementById(f[0]);
        if (input && (input.value === "" || input.value === lastDefaults[f[1]])) input.value = opt.getAttribute(f[2]);
        lastDefaults[f[1]] = opt.getAttribute(f[2]);
      });
    };
    lastDefaults.start = (document.getElementById("start_command") || {}).value || "";
    lastDefaults.output = (document.getElementById("output_dir") || {}).value || "";
    fw.addEventListener("change", function () { applyFramework(true); });
    var dbSel = document.getElementById("database");
    if (dbSel) dbSel.addEventListener("change", function () { dbSel.dataset.touched = "1"; });
    applyFramework(false);
  }

  // Logs page: start scrolled to the newest lines.
  var siteLog = document.getElementById("site-log");
  if (siteLog) siteLog.scrollTop = siteLog.scrollHeight;

  // Client-side repository filter.
  var filter = document.getElementById("repo-filter");
  if (filter) {
    filter.addEventListener("input", function () {
      var q = filter.value.trim().toLowerCase();
      document.querySelectorAll("[data-repo]").forEach(function (el) {
        el.hidden = q !== "" && el.getAttribute("data-repo").toLowerCase().indexOf(q) === -1;
      });
    });
  }

  // Picking a repository fills in the repo and its default branch.
  var pick = document.getElementById("repo-pick");
  if (pick) {
    pick.addEventListener("change", function () {
      var opt = pick.options[pick.selectedIndex];
      if (!opt || !opt.value) return;
      document.getElementById("repo").value = opt.value;
      document.getElementById("branch").value = opt.getAttribute("data-branch") || "main";
    });
  }
})();

// Sidebar drawer on small screens.
document.addEventListener("click", (e) => {
  const t = e.target.closest("[data-sidebar-toggle]");
  if (!t) return;
  document.body.classList.toggle("sb-open");
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.body.classList.remove("sb-open");
});

// Client-side filter for lists such as the sites grid.
document.querySelectorAll("input[data-filter]").forEach((input) => {
  const items = () => document.querySelectorAll(input.dataset.filter);
  input.addEventListener("input", () => {
    const q = input.value.trim().toLowerCase();
    let shown = 0;
    items().forEach((el) => {
      const hit = !q || (el.dataset.search || el.textContent).toLowerCase().includes(q);
      el.hidden = !hit;
      if (hit) shown++;
    });
    const none = document.getElementById("site-grid-none");
    if (none) none.hidden = shown > 0;
  });
});

// Server charts: crosshair + tooltip with the nearest sample.
document.querySelectorAll("svg.chart[data-points]").forEach((svg) => {
  // Each point is x:y (in viewBox units), unix time, value.
  const pts = svg.dataset.points.split(";").map((p) => p.split(":").map(Number)).filter((p) => p.length === 4);
  if (pts.length === 0) return;
  const cross = svg.querySelector(".chart-cross");
  const dot = svg.querySelector(".chart-dot");
  const tip = svg.parentElement.querySelector(".chart-tip");
  const xs = pts.map((p) => p[0]);
  const ys = pts.map((p) => p[1]);
  const hide = () => { cross.hidden = dot.hidden = tip.hidden = true; };
  svg.addEventListener("mouseleave", hide);
  svg.addEventListener("mousemove", (e) => {
    const r = svg.getBoundingClientRect();
    const x = ((e.clientX - r.left) / r.width) * 640;
    let best = 0;
    for (let i = 1; i < xs.length; i++) if (Math.abs(xs[i] - x) < Math.abs(xs[best] - x)) best = i;
    if (!pts[best]) return;
    cross.setAttribute("x1", xs[best]); cross.setAttribute("x2", xs[best]);
    dot.setAttribute("cx", xs[best]); dot.setAttribute("cy", ys[best]);
    cross.hidden = dot.hidden = tip.hidden = false;
    const t = new Date(pts[best][2] * 1000);
    tip.innerHTML = "";
    const v = document.createElement("strong");
    v.textContent = pts[best][3].toFixed(0) + "%";
    tip.append(v, " " + svg.dataset.title + " · " + t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
    const wrap = svg.parentElement.getBoundingClientRect();
    // Keep the tooltip inside the chart near its edges.
    const half = tip.offsetWidth / 2;
    const left = r.left - wrap.left + (xs[best] / 640) * r.width;
    tip.style.left = Math.max(half, Math.min(wrap.width - half, left)) + "px";
    tip.style.top = (r.top - wrap.top + (ys[best] / 150) * r.height) + "px";
  });
});
