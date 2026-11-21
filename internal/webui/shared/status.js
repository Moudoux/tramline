// Words and formats for a printer's status, shared by the read-only page and the GUI.
"use strict";

function clock(seconds) {
  if (!isFinite(seconds) || seconds < 0) return "–";
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h} h ${m} min`;
  if (m > 0) return `${m} min ${s % 60} s`;
  return `${s} s`;
}

function temp(now, target) {
  if (now === undefined) return "–";
  return target > 0 ? `${now.toFixed(1)} / ${target.toFixed(0)} °C` : `${now.toFixed(1)} °C`;
}

const STATE_TEXT = {
  heating: ["heating", "active", "Heating for the tram check before printing."],
  soaking: ["soaking", "active", "At temperature, letting the bed settle before the tram check."],
  checking: ["checking tram", "active", "Probing the corners of the bed."],
  adjusting: ["needs you", "bad", ""],
  starting: ["starting", "active", "Tram check passed, starting the file."],
  running: ["printing", "active", ""],
  cancelling: ["cancelling", "bad", "The print is being cancelled."],
  finished: ["finished", "good", ""],
  cancelled: ["cancelled", "", ""],
  scheduled: ["tram passed, scheduled", "good", ""],
  failed: ["failed", "bad", ""],
  unknown: ["ended, outcome unknown", "", ""],
};

const printing = (job) => job && (job.state === "running" || job.state === "cancelling");
const activeJob = (job) => job && ["heating", "soaking", "checking", "adjusting", "starting", "running", "cancelling"].includes(job.state);
const PREPARING = ["heating", "soaking", "checking", "adjusting", "starting", "scheduled"];

const SCHEDULE_TEXT = {
  checking: "checking tram now",
  waiting: "waiting",
  starting: "starting",
  printing: "printing",
};
const SCHEDULE_BAD = ["failed", "cancelled", "missed", "unknown"];

// In the browser's own timezone, with the date, so it can't be misread.
function scheduleWhen(at) {
  return new Date(at).toLocaleString(undefined, {
    weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", timeZoneName: "short",
  });
}

// ---- SHA-256 sums ----

// The first 12 digits, the same as the start of sha256sum's output.
const shortSum = (sha) => (sha || "").slice(0, 12);

let sumTip = null;
let sumAnchor = null;

function showSumTip(anchor) {
  if (!sumTip) {
    sumTip = document.createElement("div");
    sumTip.className = "sum-tip";
    sumTip.setAttribute("role", "tooltip");
    document.body.append(sumTip);
    // A list redrawn under the pointer takes its icon away, so the tip goes too.
    setInterval(() => { if (sumAnchor && !sumAnchor.isConnected) hideSumTip(); }, 500);
    window.addEventListener("scroll", hideSumTip, true);
  }
  sumAnchor = anchor;
  // An open modal dialog sits above everything else in the page, so the tip goes in it.
  const host = anchor.closest("dialog") || document.body;
  if (sumTip.parentNode !== host) host.append(sumTip);
  sumTip.replaceChildren(...anchor.sumTip());
  sumTip.hidden = false;
  const r = anchor.getBoundingClientRect();
  const w = sumTip.offsetWidth, h = sumTip.offsetHeight;
  const left = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), window.innerWidth - w - 8);
  const top = r.bottom + 6 + h < window.innerHeight ? r.bottom + 6 : Math.max(8, r.top - h - 6);
  sumTip.style.left = `${left}px`;
  sumTip.style.top = `${top}px`;
}

function hideSumTip() {
  if (sumTip) sumTip.hidden = true;
  sumAnchor = null;
}

// sumIcon is a small "i" to hover, focus or tap for a file's SHA-256. kind is "" for a
// sum that's fine, "bad" for one that's wrong and "unknown" for no sum, and notes are
// lines that say what the sum is of.
function sumIcon(sha, title, notes = [], kind = "") {
  const icon = document.createElement("span");
  icon.className = "sum" + (kind ? " " + kind : "");
  icon.tabIndex = 0;
  icon.setAttribute("role", "button");
  icon.textContent = kind === "bad" ? "!" : kind === "unknown" ? "?" : "i";
  icon.setAttribute("aria-label", sha ? `${title}: SHA-256 ${sha}` : `${title}: no SHA-256`);
  const canCopy = !!(sha && navigator.clipboard && window.isSecureContext);
  icon.sumTip = () => {
    const parts = [];
    const add = (cls, text) => {
      const d = document.createElement("div");
      d.className = cls;
      d.textContent = text;
      parts.push(d);
    };
    add("sum-title", title);
    if (sha) {
      add("sum-short", shortSum(sha));
      add("sum-full", sha);
    }
    for (const n of notes.filter(Boolean)) add("sum-note", n);
    if (canCopy) add("sum-note dim", icon.copied ? "Copied." : "Click to copy the whole sum.");
    return parts;
  };
  icon.addEventListener("mouseenter", () => showSumTip(icon));
  icon.addEventListener("mouseleave", () => { if (document.activeElement !== icon) hideSumTip(); });
  icon.addEventListener("focus", () => showSumTip(icon));
  icon.addEventListener("blur", hideSumTip);
  icon.addEventListener("click", async (e) => {
    // Rows open when clicked, which this shouldn't do.
    e.stopPropagation();
    if (canCopy) {
      try {
        await navigator.clipboard.writeText(sha);
        icon.copied = true;
      } catch (_) { /* the tip still shows the sum to copy by hand */ }
    }
    showSumTip(icon);
  });
  icon.addEventListener("keydown", (e) => {
    if (e.key === "Escape") icon.blur();
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      icon.click();
    }
  });
  return icon;
}

// clockAt is a time of day for a tip, in the browser's timezone.
function clockAt(at) {
  return new Date(at).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

// jobSum is the icon for a print's file, or nothing for one with no sum.
function jobSum(job) {
  if (!job) return null;
  if (!job.sha256) {
    if (job.elsewhere) return null;
    return sumIcon("", "The file this prints", [job.mode === "sd"
      ? "Its copy on the card was made before checksums were kept."
      : "Started before checksums were kept."], "unknown");
  }
  return sumIcon(job.sha256, "The file this prints", [job.mode === "sd"
    ? "What the Pi sent to the card in its copy, whose size then checked out. The card can't be read back."
    : "Checked on the Pi's disk just before it started, against its upload."]);
}

// setFileLine shows a name with an icon for its sum, and is left alone when nothing
// changed, so a tip that's open stays open.
function setFileLine(el, name, icon, key) {
  const k = JSON.stringify([name, key]);
  if (el.dataset.key === k) return;
  el.dataset.key = k;
  if (!name) {
    el.replaceChildren();
    return;
  }
  el.replaceChildren(document.createTextNode(name));
  if (icon) el.append(" ", icon);
}

// ---- Following the server's status ----

// The server pushes its status as server-sent events, within a fraction of a second of
// a change and at least once a second, so a stream that's quiet for a while has stalled.
const STREAM_QUIET_MS = 5000;
const STREAM_STALE_MS = 15000;
const STREAM_RETRY_MS = 2000;

// followStatus gives onStatus each status from the event stream at url. If the server
// has no stream, as an older Tramline doesn't, it falls back to calling poll every
// pollMs, which may be a function, or hiddenPollMs with the tab hidden. With no pollMs
// it only polls once, to tell an older server from one out of reach, and leaves the
// rest to the caller, who can see that from streaming. The stream is
// closed while the tab is hidden. onLost is told, with the error if there is one, when
// the server can't be reached, and done says when to give up for good.
function followStatus(o) {
  let source = null;
  let streamed = false;
  let polling = false;
  let last = 0;
  let timer = 0;
  const done = o.done || (() => false);
  const got = (st) => { last = Date.now(); o.onStatus(st); };

  function open() {
    if (done() || polling || source || document.hidden) return;
    const es = new EventSource(o.url);
    source = es;
    last = Date.now();
    es.addEventListener("status", (e) => {
      let st;
      try { st = JSON.parse(e.data); } catch { return; }
      streamed = true;
      got(st);
    });
    es.onerror = () => {
      if (source !== es) return;
      // Still open means the browser is reconnecting by itself.
      if (es.readyState !== EventSource.CLOSED) { o.onLost(); return; }
      close();
      if (streamed) { o.onLost(); setTimeout(open, STREAM_RETRY_MS); return; }
      // Refused before it ever sent anything: either the server is out of reach, or it
      // has no stream. A poll tells which.
      firstPoll();
    };
  }

  function close() {
    if (source) source.close();
    source = null;
  }

  async function firstPoll() {
    if (!o.poll) { o.onLost(); setTimeout(open, STREAM_RETRY_MS); return; }
    try {
      got(await o.poll());
    } catch (err) {
      o.onLost(err);
      if (!done()) setTimeout(open, STREAM_RETRY_MS);
      return;
    }
    polling = true;
    if (o.pollMs) timer = setTimeout(pollOnce, interval());
  }

  const interval = () => {
    if (document.hidden && o.hiddenPollMs) return o.hiddenPollMs;
    return typeof o.pollMs === "function" ? o.pollMs() : o.pollMs;
  };

  async function pollOnce() {
    clearTimeout(timer);
    if (done()) return;
    try { got(await o.poll()); } catch (err) { o.onLost(err); }
    if (!done()) timer = setTimeout(pollOnce, interval());
  }

  setInterval(() => {
    if (!source || done()) return;
    const quiet = Date.now() - last;
    if (quiet > STREAM_STALE_MS) { close(); open(); }
    else if (quiet > STREAM_QUIET_MS) o.onLost();
  }, 1000);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) close();
    else if (polling) { if (o.pollMs) pollOnce(); }
    else open();
  });
  open();

  return {
    // streaming is false once it has fallen back to polling.
    get streaming() { return !polling; },
    // refresh asks again soon, for after an action. A stream needs nothing.
    refresh() {
      if (!polling || !o.pollMs) return;
      clearTimeout(timer);
      timer = setTimeout(pollOnce, 300);
    },
  };
}
