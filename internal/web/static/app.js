(function () {
  "use strict";

  // Ask before destructive form submissions: <form data-confirm="..."> or <button data-confirm="...">.
  document.addEventListener("submit", function (e) {
    var el = e.submitter && e.submitter.getAttribute("data-confirm") ? e.submitter : e.target;
    var msg = el.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

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
          status.textContent = d.status;
          status.className = "badge badge-" + d.status;
          if (d.duration) duration.textContent = "· " + d.duration;
          if (d.link) {
            var next = document.getElementById("job-next");
            var a = document.getElementById("job-next-link");
            a.href = d.link;
            a.textContent = "Open the panel at " + d.link.replace(/\/settings$/, "");
            next.hidden = false;
          }
          if (!d.done || d.more) setTimeout(poll, d.more ? 50 : 1000);
        })
        .catch(function () { setTimeout(poll, 3000); });
    };
    poll();
  }

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
