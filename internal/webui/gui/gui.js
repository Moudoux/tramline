// Tramline's GUI. Everything goes through tramline gui on this machine, which forwards
// it to the Pi with the client's certificate. The Pi's rules decide what's allowed, and
// its error messages are shown as they come.
"use strict";

const $ = (id) => document.getElementById(id);

// The page's status is cheap, from the Pi's memory. The full status asks the Hue Bridge
// for the plug's state every time, so it's asked for less often.
const PAGE_MS = 1000;
const FULL_MS = 5000;
const FULL_BUSY_MS = 2000;
const UPLOAD_POLL_MS = 400;
const ESTOP_HOLD_MS = 600;
const FILE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const CORNERS = ["front-left", "front-right", "back-right", "back-left"];
// The bed map is drawn as seen from above, with the back at the top.
const MAP_ORDER = [3, 2, 0, 1];
const SCHEDULE_PENDING = ["checking", "waiting", "starting"];

let info = { tram_tolerance: 0.05 };
let page = null;
let full = null;
let files = null;
let schedules = null;
let lastCopyState = null;
let fullTimer = 0;
let gone = false;

const view = new PrintView($("view"), (id) => `/pi/model/${id}`);
if (!view.ok) {
  $("no-webgl").hidden = false;
  $("view").hidden = true;
}

// The panel covers the left of the view, so the bed is centred in what's left.
function placeView() {
  if (!view.ok) return;
  const side = $("side");
  const beside = getComputedStyle(side).position === "fixed";
  view.renderer.offsetX = beside ? (side.offsetLeft + side.offsetWidth) / 2 : 0;
  view.dirty = true;
}
window.addEventListener("resize", placeView);
placeView();

// ---- Talking to tramline gui ----

class Gone extends Error {}

async function api(method, path, body) {
  const opts = { method, cache: "no-store", headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, opts);
  } catch (err) {
    signedOut("tramline gui has stopped", "Run tramline gui again to carry on. Anything already sent to the Pi carries on there.");
    throw new Gone("tramline gui isn't running");
  }
  if (resp.status === 401) {
    signedOut("This tab isn't signed in", "Run tramline gui, which opens a signed-in tab. Each sign-in link works once.");
    throw new Gone("not signed in");
  }
  const text = await resp.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = text; }
  }
  if (!resp.ok) throw new Error((data && data.error) || `${resp.status} ${resp.statusText}`);
  return data;
}

function signedOut(title, text) {
  if (gone) return;
  gone = true;
  $("signed-out-title").textContent = title;
  $("signed-out-text").textContent = text;
  $("signed-out").hidden = false;
}

// ---- Small helpers ----

function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

function button(label, className, onClick) {
  const b = el("button", "btn " + (className || ""), label);
  b.type = "button";
  b.addEventListener("click", (e) => {
    e.stopPropagation();
    onClick(b);
  });
  return b;
}

function bytes(n) {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KB`;
  return `${n} bytes`;
}

function shortTime(t) {
  return new Date(t).toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

function clockTime(t) {
  return new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

// sentence starts a message with a capital and ends it with a full stop, as the Pi's
// messages are written to follow other words.
function sentence(text) {
  const t = String(text).trim();
  if (!t) return t;
  return t[0].toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? "" : ".");
}

function toast(text, kind, ms) {
  const t = el("div", "toast " + (kind || ""), sentence(text));
  const close = () => t.remove();
  t.addEventListener("click", close);
  $("toasts").append(t);
  setTimeout(close, ms || (kind === "bad" ? 12000 : 5000));
}

// act runs an action from a button: the button shows it's working, and the outcome is
// shown either way. It gives back the answer, or undefined if it failed.
async function act(btn, run, done) {
  if (btn) {
    btn.disabled = true;
    btn.classList.add("working");
  }
  try {
    const out = await run();
    const text = typeof done === "function" ? done(out) : done;
    if (text) toast(text, "good");
    refreshSoon();
    return out === undefined ? true : out;
  } catch (err) {
    if (!(err instanceof Gone)) toast(err.message, "bad");
    return undefined;
  } finally {
    if (btn) {
      btn.classList.remove("working");
      btn.disabled = false;
      applyBusy();
    }
  }
}

// ---- Dialogs ----

function closeOnButtons(dialog) {
  dialog.querySelectorAll("[data-close]").forEach((b) => b.addEventListener("click", () => dialog.close()));
}

for (const d of document.querySelectorAll("dialog")) closeOnButtons(d);

// ask asks a yes or no question. Danger makes the yes button red.
function ask({ title, text, yes = "OK", no = "Cancel", danger = false, list = null, cancel = true }) {
  const d = $("dlg-confirm");
  $("confirm-title").textContent = title;
  $("confirm-text").textContent = text || "";
  $("confirm-text").hidden = !text;
  $("confirm-list").replaceChildren(...(list || []).map((x) => el("li", "", x)));
  $("confirm-list").hidden = !list || list.length === 0;
  const yesBtn = $("confirm-yes");
  yesBtn.textContent = yes;
  yesBtn.className = "btn primary" + (danger ? " danger" : "");
  $("confirm-no").hidden = !cancel;
  $("confirm-no").textContent = no;
  return new Promise((resolve) => {
    const finish = (answer) => {
      yesBtn.removeEventListener("click", onYes);
      d.removeEventListener("close", onClose);
      resolve(answer);
    };
    const onYes = () => {
      d.close();
      finish(true);
    };
    const onClose = () => finish(false);
    yesBtn.addEventListener("click", onYes);
    d.addEventListener("close", onClose);
    d.showModal();
    (danger ? $("confirm-no") : yesBtn).focus();
  });
}

// ---- What's going on ----

const tramming = () => full && full.tram && full.tram.state === "running";
const copying = () => (page && page.copy) || (full && full.copy && full.copy.state === "running");
const printBusy = () => page && activeJob(page.print);

// busyReason says why a new job can't start, or "" if it can.
function busyReason() {
  if (printBusy()) return "A print is under way.";
  if (tramming()) return "A tram check is running.";
  if (copying()) return "A copy to the card is running.";
  return "";
}

// applyBusy greys out what can't start now. The Pi refuses it anyway, this only saves a
// click.
function applyBusy() {
  const reason = busyReason();
  for (const id of ["preheat", "tram-start", "schedule-new"]) {
    const b = $(id);
    if (b.classList.contains("working")) continue;
    b.disabled = Boolean(reason);
    b.title = reason;
  }
  for (const b of document.querySelectorAll("[data-needs-idle]")) {
    b.disabled = Boolean(reason);
    b.title = reason;
  }
}

function bedMap(relative, target, pending) {
  const map = el("div", "bedmap");
  for (const i of MAP_ORDER) {
    const c = el("div", "corner");
    c.append(el("small", "", CORNERS[i]));
    const v = relative ? relative[i] : undefined;
    if (v === undefined || v === null || pending) {
      c.classList.add("pending");
      c.append(el("b", "", pending && pending[i] !== undefined ? `${pending[i].toFixed(3)} mm` : "–"));
    } else {
      c.append(el("b", "", `${v >= 0 ? "+" : ""}${v.toFixed(3)} mm`));
    }
    if (target === CORNERS[i]) c.classList.add("target");
    map.append(c);
  }
  return map;
}

function turnOf(action) {
  return action === "lower" ? "counter-clockwise" : "clockwise";
}

function verdict(result) {
  const v = el("div", "verdict");
  v.append(el("span", "chip " + (result.pass ? "good" : "bad"), result.pass ? "in tram" : "out of tram"));
  v.append(el("span", "hint", `range ${result.range_mm.toFixed(3)} mm, limit ${info.tram_tolerance.toFixed(2)} mm`));
  return v;
}

function advice(result) {
  const a = el("p", "advice");
  const corner = result.corner;
  a.append(el("strong", "", `${result.action[0].toUpperCase()}${result.action.slice(1)} ${corner}: `));
  a.append(document.createTextNode(`turn its knob ${turnOf(result.action)}, seen from above.`));
  return a;
}

// showNow fills the top card from the page status, and the job's own buttons.
function showNow() {
  const st = page;
  if (!st) return;
  const job = st.print;
  const badge = $("badge");
  let text, kind, message = "";
  const offText = full && full.plug && full.plug.on ? "The plug is on, and the printer is connecting." : "The printer is off.";
  // focus is the job the card is about: a print, unless something else is running.
  let focus = null;
  if (activeJob(job)) {
    focus = job;
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "active", ""];
    if (job.state === "adjusting") {
      message = job.parked ? "The head is parked. The bed may be hot." : "The head may be low. The bed may be hot.";
      message += " Adjust the knob, then check again.";
    }
  } else if (st.tramming || tramming()) {
    [text, kind] = ["checking tram", "active"];
    message = full && full.tram && full.tram.step ? `Tram check: ${full.tram.step}.` : "Probing the corners of the bed.";
  } else if (st.copy) {
    [text, kind] = ["copying to card", "active"];
    const pct = st.copy.total_bytes > 0 ? Math.floor((100 * st.copy.bytes) / st.copy.total_bytes) : 0;
    message = `Copying ${st.copy.name} to the card: ${pct}%.`;
  } else if (job) {
    focus = job;
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "", ""];
    if (!st.connected) message = offText;
  } else if (!st.connected) {
    [text, kind] = ["printer off", ""];
    message = "The printer is off, or not connected.";
  } else {
    [text, kind] = ["idle", ""];
  }
  badge.textContent = text;
  badge.className = "badge" + (kind ? " " + kind : "");

  $("file").textContent = focus ? focus.name : st.copy ? st.copy.name : "";
  $("nozzle").textContent = st.temps ? temp(st.temps.nozzle, st.temps.nozzle_target) : "–";
  $("bed").textContent = st.temps ? temp(st.temps.bed, st.temps.bed_target) : "–";

  const notes = [message];
  if (focus && focus.note) notes.push(sentence(focus.note));
  if (focus && focus.stopping) notes.push("Being cancelled.");
  if (focus && focus.link_lost) notes.push("The link to the printer is lost, so this is out of date.");
  if (focus && focus.elsewhere) notes.push("Started from the printer, not from here.");
  if (focus && view.modelMissing) notes.push("Its file isn't on the Pi, so there's no 3D view.");
  const shown = notes.filter(Boolean).join(" ");
  $("message").textContent = shown;
  $("message").hidden = !shown;

  const problems = [];
  if (focus && focus.error && !["finished", "cancelled"].includes(focus.state)) problems.push(focus.error);
  if (full && full.heat_problem) problems.push(full.heat_problem);
  $("problem").textContent = problems.map(sentence).join(" ");
  $("problem").hidden = problems.length === 0;

  const showProgress = focus === job && job && job.size > 0 && (job.pos > 0 || printing(job)) && !PREPARING.includes(job.state);
  $("progress-block").hidden = !showProgress;
  if (showProgress) {
    const pct = Math.min((100 * job.pos) / job.size, 100);
    $("bar-fill").style.width = `${pct}%`;
    $("percent").textContent = `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
    $("elapsed").textContent = clock(job.run_seconds);
  }
  showDetails();

  // The failed check the print is waiting on.
  const card = $("check-card");
  if (job && job.state === "adjusting" && job.check && !job.check.pass) {
    card.replaceChildren(bedMap(job.check.relative_mm, job.check.corner), verdict(job.check), advice(job.check));
    card.hidden = false;
  } else {
    card.hidden = true;
  }

  showJobActions(job);
  applyBusy();

  const busyNow = busyReason();
  if (busyNow !== lastBusy) {
    if (lastBusy !== null && files) loadFiles();
    lastBusy = busyNow;
  }
}

let lastBusy = null;

function showDetails() {
  const d = view.ok ? view.details() : { layer: "–", left: "–" };
  $("layer").textContent = d.layer;
  $("left").textContent = d.left;
}

// The buttons that belong to what's running now.
let shownActions = "";
function showJobActions(job) {
  const box = $("job-actions");
  let key = "";
  if (activeJob(job)) key = job.state === "adjusting" ? "adjusting" : job.state === "cancelling" || job.stopping ? "" : "print";
  else if (page && page.copy) key = "copy";
  if (key === shownActions) return;
  shownActions = key;
  const buttons = [];
  if (key === "adjusting") {
    buttons.push(button("Check again", "primary", (b) => act(b, () => api("POST", "/v1/print/continue"), "Checking tram again. The head will move.")));
  }
  if (key === "adjusting" || key === "print") {
    buttons.push(button("Cancel print", "danger", cancelPrint));
  }
  if (key === "copy") {
    buttons.push(button("Stop copy", "danger", (b) => act(b, () => api("POST", "/v1/copy/cancel"), "Stopping the copy.")));
  }
  box.replaceChildren(...buttons);
  box.hidden = buttons.length === 0;
}

async function cancelPrint(b) {
  const job = page && page.print;
  const ok = await ask({
    title: "Cancel the print?",
    text: `${job ? job.name + ": t" : "T"}he heaters turn off. From the card the head goes home, from the Pi it lifts 5 mm.`,
    yes: "Cancel print",
    no: "Keep printing",
    danger: true,
  });
  if (ok) act(b, () => api("POST", "/v1/cancel"), "Cancelling. The status confirms it.");
}

// showFull fills in what only the full status has: the plug, heaters, tram and firmware.
function showFull() {
  const st = full;
  if (!st) return;
  const plug = st.plug;
  const plugDD = $("plug");
  let plugText = "–";
  plugDD.className = "";
  if (plug && plug.error) {
    plugText = "unknown";
    $("plug-text").textContent = `Can't read the plug: ${plug.error}`;
  } else if (plug) {
    plugText = plug.on ? "on" : "off";
    plugDD.className = plug.on ? "on" : "off";
    $("plug-text").textContent = `${plug.name || "Plug"} is ${plugText}`;
  } else {
    $("plug-text").textContent = "No plug is set up on the server.";
  }
  plugDD.textContent = plugText;
  $("power-on").classList.toggle("selected", Boolean(plug && plug.on));
  $("power-on").classList.toggle("on", Boolean(plug && plug.on));
  $("power-off").classList.toggle("selected", Boolean(plug && !plug.on && !plug.error));

  const heat = [];
  if (st.heaters_off_at) heat.push(`The heaters turn off at ${clockTime(st.heaters_off_at)} unless you preheat again.`);
  $("heat-note").textContent = heat.join(" ");
  $("heat-note").hidden = heat.length === 0;

  $("firmware").textContent = st.printer.connected
    ? `${st.printer.firmware || "Connected"}${st.simulated ? " (simulated printer)" : ""}`
    : `Not connected${st.simulated ? " (simulated printer)" : ""}`;
  $("about-server").textContent = `${st.version}${st.simulated ? ", simulated printer" : ""}`;

  showTram(st.tram);

  // A copy that just ended changes the files' state.
  const copyState = st.copy ? `${st.copy.id}:${st.copy.state}` : null;
  if (copyState !== lastCopyState) {
    if (lastCopyState !== null && st.copy && st.copy.state !== "running") {
      if (st.copy.state === "failed") toast(`Copy of ${st.copy.name} failed: ${st.copy.error}`, "bad");
      else toast(`${st.copy.name} is on the card as ${st.copy.sd_name}.`, "good");
    }
    lastCopyState = copyState;
    if (files) loadFiles();
  }

  const coming = (st.scheduled || []).length;
  $("schedule-count").textContent = String(coming);
  $("schedule-count").hidden = coming === 0;
  if (!$("tab-schedule").hidden) loadSchedules();
  showNow();
}

function showTram(job) {
  const box = $("tram-view");
  if (!job) {
    box.replaceChildren(el("p", "hint", "No check since the server started."));
    return;
  }
  const parts = [];
  if (job.state === "running") {
    const round = job.rounds && job.rounds.length ? job.rounds[job.rounds.length - 1] : null;
    const so = {};
    if (round) for (const r of round.readings) so[CORNERS.indexOf(r.corner)] = r.z;
    parts.push(el("p", "hint", `Running${job.repeat > 1 ? `, round ${job.rounds.length} of ${job.repeat}` : ""}: ${job.step || "starting"}.`));
    parts.push(bedMap(null, null, so));
  } else if (job.state === "failed") {
    parts.push(el("p", "problem", `The last check failed: ${job.error}`));
  } else if (job.result) {
    parts.push(bedMap(job.result.relative_mm, job.result.pass ? null : job.result.corner));
    parts.push(verdict(job.result));
    if (!job.result.pass) parts.push(advice(job.result));
    if (job.repeat > 1 && job.spread_mm) {
      parts.push(el("p", "hint", `Repeatability over ${job.repeat} rounds: ` + job.spread_mm.map((s, i) => `${CORNERS[i]} ${s.toFixed(3)} mm`).join(", ") + "."));
    }
    const when = job.finished_at ? `Checked ${shortTime(job.finished_at)}. ` : "";
    const parked = job.parked ? "The head is parked." : "The head was not parked.";
    parts.push(el("p", "hint", when + parked));
  }
  box.replaceChildren(...parts);
}

// ---- Polling ----

async function pollPage() {
  if (gone) return;
  try {
    page = await api("GET", "/pi/status");
    if (view.ok) view.update(page.print);
    showNow();
  } catch (err) {
    if (!(err instanceof Gone)) {
      $("badge").textContent = "can't reach the Pi";
      $("badge").className = "badge bad";
    }
  }
  setTimeout(pollPage, document.hidden ? PAGE_MS * 5 : PAGE_MS);
}

async function pollFull() {
  clearTimeout(fullTimer);
  if (gone) return;
  if (!document.hidden) {
    try {
      full = await api("GET", "/v1/status");
      showFull();
    } catch (err) {
      if (!(err instanceof Gone)) {
        $("problem").textContent = err.message;
        $("problem").hidden = false;
      }
    }
  }
  const busy = tramming() || copying() || (page && page.print && PREPARING.includes(page.print.state));
  fullTimer = setTimeout(pollFull, busy ? FULL_BUSY_MS : FULL_MS);
}

// refreshSoon catches up after an action, without waiting for the next poll.
function refreshSoon() {
  clearTimeout(fullTimer);
  fullTimer = setTimeout(pollFull, 300);
  if (files) setTimeout(loadFiles, 300);
}

setInterval(showDetails, 500);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) pollFull();
});

// ---- Tabs ----

// Each tab keeps its own scroll position, as they share one scrolling area.
const tabScroll = {};
let currentTab = "files";
for (const tab of document.querySelectorAll(".tabs button")) {
  tab.addEventListener("click", () => {
    const name = tab.dataset.tab;
    if (name === currentTab) return;
    const panes = document.querySelector(".panes");
    tabScroll[currentTab] = panes.scrollTop;
    currentTab = name;
    for (const t of document.querySelectorAll(".tabs button")) t.setAttribute("aria-selected", String(t === tab));
    for (const pane of document.querySelectorAll(".pane")) pane.hidden = pane.id !== `tab-${name}`;
    panes.scrollTop = tabScroll[name] || 0;
    if (name === "files") loadFiles();
    if (name === "schedule") loadSchedules();
  });
}

// ---- Files ----

let filesLoading = false;
async function loadFiles() {
  if (filesLoading) return;
  filesLoading = true;
  $("files-refresh").classList.add("spinning");
  try {
    files = await api("GET", "/v1/files");
    showFiles();
  } catch (err) {
    if (!(err instanceof Gone)) {
      $("pi-files").replaceChildren(el("li", "empty", `Can't list the files: ${err.message}`));
    }
  } finally {
    filesLoading = false;
    $("files-refresh").classList.remove("spinning");
  }
}

$("files-refresh").addEventListener("click", loadFiles);

let openFile = null;

function cardState(rec) {
  const running = page && page.copy && page.copy.name === rec.name;
  if (running) {
    const pct = page.copy.total_bytes > 0 ? Math.floor((100 * page.copy.bytes) / page.copy.total_bytes) : 0;
    return ["copying " + pct + "%", "active"];
  }
  switch (rec.sd_state) {
    case "done": return ["on card", "good"];
    case "running": return ["copying", "active"];
    case "failed": return ["copy failed", "bad"];
  }
  return ["Pi only", ""];
}

let shownFiles = "";
function showFiles() {
  // Rebuilding the list loses hover and focus, so it's only done for a change.
  const key = JSON.stringify([files, openFile, page && page.copy && [page.copy.name, page.copy.bytes]]);
  if (key === shownFiles) {
    applyBusy();
    return;
  }
  shownFiles = key;
  const list = files.files || [];
  const sorted = [...list].sort((a, b) => new Date(b.uploaded_at) - new Date(a.uploaded_at));
  if (sorted.length === 0) {
    $("pi-files").replaceChildren(el("li", "empty", "No files on the Pi yet. Drop one above."));
  } else {
    $("pi-files").replaceChildren(...sorted.map(fileItem));
  }

  $("card-note").hidden = files.card !== null && files.card !== undefined;
  $("card-note").textContent = files.card ? "" : `Can't read the card now: ${files.card_note || "the printer is busy or off"}.`;
  const ours = new Set(list.map((r) => (r.sd_name || "").toUpperCase()));
  const card = files.card || [];
  if (files.card && card.length === 0) {
    $("card-files").replaceChildren(el("li", "empty", "Nothing on the card."));
  } else {
    $("card-files").replaceChildren(...card.map((f) => {
      const li = el("li", "compact");
      const main = el("div", "item-main");
      const text = el("div", "item-text");
      text.append(el("span", "item-name", f.name), el("span", "item-meta", bytes(f.size)));
      main.append(text, el("span", "chip" + (ours.has(f.name.toUpperCase()) ? " good" : ""), ours.has(f.name.toUpperCase()) ? "from here" : "not from here"));
      li.append(main);
      return li;
    }));
  }
  applyBusy();
}

function fileItem(rec) {
  const li = el("li", "item");
  if (openFile === rec.name) li.classList.add("open");
  const main = el("div", "item-main");
  const text = el("div", "item-text");
  const meta = [bytes(rec.size), `${rec.info.lines.toLocaleString()} lines`, `uploaded ${shortTime(rec.uploaded_at)}`];
  text.append(el("span", "item-name", rec.name), el("span", "item-meta", meta.join(" · ")));
  if (rec.sd_state === "failed" && rec.sd_error) text.append(el("span", "item-why bad", rec.sd_error));
  const [chipText, chipKind] = cardState(rec);
  main.append(text, el("span", "chip " + chipKind, chipText));
  li.append(main);

  const actions = el("div", "item-actions");
  const printBtn = button("Print…", "primary small", () => openRun("print", rec.name));
  const schedBtn = button("Schedule…", "small", () => openRun("schedule", rec.name));
  const onCard = rec.sd_state === "done";
  const copyBtn = button(rec.sd_state === "failed" ? "Copy again" : "Copy to card", "small", (b) => copyFile(b, rec));
  const delBtn = button("Delete", "danger small", (b) => deleteFile(b, rec));
  for (const b of [printBtn, schedBtn, copyBtn, delBtn]) b.dataset.needsIdle = "";
  actions.append(printBtn, schedBtn);
  if (!onCard) actions.append(copyBtn);
  actions.append(delBtn);
  li.append(actions);

  li.addEventListener("click", () => {
    openFile = openFile === rec.name ? null : rec.name;
    for (const other of $("pi-files").children) if (other !== li) other.classList.remove("open");
    li.classList.toggle("open", openFile === rec.name);
  });
  return li;
}

async function copyFile(b, rec) {
  const seconds = Math.max(Math.round(rec.info.sd_size / (info.copy_bytes_per_second || 14000)), 1);
  const ok = await ask({
    title: `Copy ${rec.name} to the card?`,
    text: `It takes about ${clock(seconds)}, with the printer on, and nothing else can run meanwhile. Printing from the card doesn't depend on the Pi once it starts.`,
    yes: "Copy",
  });
  if (!ok) return;
  act(b, () => api("POST", `/v1/files/${encodeURIComponent(rec.name)}/copy`, {}), (out) =>
    out && out.job ? `Copying ${rec.name} to the card.` : `${rec.name} is already on the card.`);
}

async function deleteFile(b, rec) {
  const ok = await ask({
    title: `Delete ${rec.name}?`,
    text: rec.on_card ? "It's deleted from the Pi, and its copy on the card too." : "It's deleted from the Pi.",
    yes: "Delete",
    danger: true,
  });
  if (!ok) return;
  if (openFile === rec.name) openFile = null;
  act(b, () => api("DELETE", `/v1/files/${encodeURIComponent(rec.name)}`), `Deleted ${rec.name}.`);
}

// ---- Uploading ----

const drop = $("drop");
drop.addEventListener("keydown", (e) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    $("file-input").click();
  }
});
$("file-input").addEventListener("change", () => {
  const f = $("file-input").files[0];
  $("file-input").value = "";
  if (f) upload(f);
});

let dragDepth = 0;
const hasFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");
window.addEventListener("dragenter", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth++;
  $("drag-overlay").hidden = false;
});
window.addEventListener("dragover", (e) => {
  if (hasFiles(e)) e.preventDefault();
});
window.addEventListener("dragleave", () => {
  dragDepth = Math.max(dragDepth - 1, 0);
  if (dragDepth === 0) $("drag-overlay").hidden = true;
});
window.addEventListener("drop", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $("drag-overlay").hidden = true;
  const list = [...e.dataTransfer.files];
  if (list.length !== 1) {
    toast("Drop one file at a time.", "bad");
    return;
  }
  upload(list[0]);
});

// safeName turns a name the Pi won't take into one it will, keeping the extension.
function safeName(name) {
  let stem = name.replace(/\.(gcode|gco)$/i, "");
  const ext = name.slice(stem.length).toLowerCase() || ".gcode";
  stem = stem.replace(/[^A-Za-z0-9._-]+/g, "_").replace(/^[^A-Za-z0-9]+/, "").replace(/[._-]+$/, "");
  stem = stem.slice(0, 64 - ext.length) || "print";
  return stem + ext;
}

let uploading = false;

async function upload(f) {
  if (uploading) {
    toast("An upload is already running.", "bad");
    return;
  }
  if (!/\.(gcode|gco)$/i.test(f.name)) {
    toast(`${f.name} isn't G-code: the Pi takes .gcode or .gco files.`, "bad");
    return;
  }
  if (info.max_upload && f.size > info.max_upload) {
    toast(`${f.name} is ${bytes(f.size)}, more than the ${bytes(info.max_upload)} the Pi takes.`, "bad");
    return;
  }
  let name = f.name;
  if (!FILE_NAME.test(name)) {
    name = safeName(name);
    const ok = await ask({
      title: "Upload under a new name?",
      text: `The Pi only takes names of letters, digits, dots, dashes and underscores, up to 64. It would be saved as ${name}.`,
      yes: "Upload",
    });
    if (!ok) return;
  }
  const existing = files && (files.files || []).find((r) => r.name === name);
  if (existing) {
    const ok = await ask({
      title: `Replace ${name}?`,
      text: "A file by this name is already on the Pi. If it's the same file nothing is sent. If it's different, the Pi's copy is replaced.",
      yes: "Upload",
    });
    if (!ok) return;
  }

  uploading = true;
  const box = $("upload");
  box.hidden = false;
  box.className = "upload";
  $("upload-name").textContent = name;
  const phase = (text, pct) => {
    $("upload-phase").textContent = text;
    $("upload-fill").style.width = `${pct}%`;
  };
  phase("starting", 0);

  // Progress to the Pi comes from tramline gui, which is what sends it there.
  let following = true;
  const follow = async () => {
    while (following) {
      try {
        const u = await api("GET", "/gui/upload");
        if (u && u.name === name && u.phase === "sending" && u.total > 0) {
          const pct = (100 * u.bytes) / u.total;
          phase(`sending to the Pi, ${Math.floor(pct)}%`, 10 + 0.9 * pct);
        }
      } catch { /* the upload's own answer says what happened */ }
      await new Promise((r) => setTimeout(r, UPLOAD_POLL_MS));
    }
  };

  try {
    const result = await new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/gui/upload?name=${encodeURIComponent(name)}`);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.upload.onprogress = (e) => {
        if (!e.lengthComputable) return;
        const pct = (100 * e.loaded) / e.total;
        phase(pct < 100 ? `reading, ${Math.floor(pct)}%` : "checking", pct / 10);  // reading is the first tenth
        if (pct >= 100) follow();
      };
      xhr.onload = () => {
        let data = null;
        try { data = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
        if (xhr.status === 401) {
          signedOut("This tab isn't signed in", "Run tramline gui, which opens a signed-in tab.");
          reject(new Gone("not signed in"));
        } else if (xhr.status !== 200) {
          reject(new Error((data && data.error) || `${xhr.status} ${xhr.statusText}`));
        } else {
          resolve(data);
        }
      };
      xhr.onerror = () => reject(new Error("tramline gui stopped answering"));
      xhr.send(f);
    });
    box.classList.add("done");
    if (result.already) {
      phase("already on the Pi", 100);
      toast(`${name} is already on the Pi, so it wasn't sent again.`, "good");
    } else {
      phase("on the Pi", 100);
      toast(`${name} is on the Pi: ${result.file.info.lines.toLocaleString()} commands.`, "good");
    }
    openFile = name;
    loadFiles();
    setTimeout(() => { if (box.classList.contains("done")) box.hidden = true; }, 6000);
  } catch (err) {
    box.classList.add("failed");
    phase("failed", 100);
    if (!(err instanceof Gone)) toast(`Upload of ${name} failed: ${err.message}`, "bad");
  } finally {
    following = false;
    uploading = false;
  }
}

// ---- Print and schedule ----

let runMode = "print";

function openRun(mode, name) {
  runMode = mode;
  const d = $("dlg-run");
  $("run-title").textContent = mode === "print" ? "Print" : "Schedule a print";
  $("run-go").textContent = mode === "print" ? "Start print" : "Schedule";
  $("run-when").hidden = mode !== "schedule";
  $("run-clear-text").textContent = mode === "print"
    ? "I've checked the bed is clear"
    : "I've checked the bed is clear, and nothing will go on it before the print";
  $("run-error").hidden = true;
  $("run-clear").checked = false;
  for (const r of document.querySelectorAll('input[name="run-from"]')) r.checked = false;

  const list = (files && files.files) || [];
  const select = $("run-file");
  select.replaceChildren(...list.map((r) => {
    const o = el("option", "", r.name);
    o.value = r.name;
    return o;
  }));
  if (name) select.value = name;
  if (list.length === 0) {
    toast("Upload a file first.", "bad");
    return;
  }

  if (mode === "schedule") {
    const lead = (info.schedule_min_lead_minutes || 15) + 5;
    const t = new Date(Date.now() + Math.max(lead, 60) * 60000);
    t.setMinutes(Math.ceil(t.getMinutes() / 5) * 5, 0, 0);
    const local = new Date(t.getTime() - t.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    $("run-at").value = local;
    $("run-at").min = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  }
  updateRun();
  d.showModal();
}

function updateRun() {
  const list = (files && files.files) || [];
  const rec = list.find((r) => r.name === $("run-file").value);
  const onCard = rec && rec.sd_state === "done";
  const [pi, sd] = document.querySelectorAll('input[name="run-from"]');
  sd.disabled = !onCard;
  $("run-sd-note").textContent = onCard ? `Prints the copy on the card, ${rec.sd_name}.` : "Copy it to the card first, from the file list.";
  // With both possible, it's the user's choice, as it is in the CLI.
  if (!onCard) {
    sd.checked = false;
    pi.checked = true;
  }
  const from = document.querySelector('input[name="run-from"]:checked');
  const steps = runMode === "print"
    ? "Turns the printer on if needed, heats to bed 80 °C and nozzle 150 °C, checks tram, then prints. It turns off once the print is over and cool."
    : "Heats and checks tram now, so the head will move. On a pass the printer turns off until the time, then powers on, checks tram again and prints only if it still passes. It ends turned off.";
  $("run-steps").textContent = steps;
  $("run-go").disabled = !($("run-clear").checked && from && rec && (runMode === "print" || $("run-at").value));
}

$("run-file").addEventListener("change", updateRun);
$("run-clear").addEventListener("change", updateRun);
$("run-at").addEventListener("input", updateRun);
for (const r of document.querySelectorAll('input[name="run-from"]')) r.addEventListener("change", updateRun);

$("run-go").addEventListener("click", async () => {
  const name = $("run-file").value;
  const from = document.querySelector('input[name="run-from"]:checked').value;
  const body = { name, from, bed_clear: $("run-clear").checked };
  const b = $("run-go");
  b.disabled = true;
  b.classList.add("working");
  try {
    if (runMode === "print") {
      const job = await api("POST", "/v1/print", body);
      toast(job.mode === "stream"
        ? `Heating for the tram check. ${job.name} streams from the Pi once that passes.`
        : `Heating for the tram check. ${job.name} starts from the card once that passes.`, "good", 8000);
    } else {
      body.at = new Date($("run-at").value).toISOString();
      const p = await api("POST", "/v1/schedule", body);
      toast(`Scheduled ${p.name} for ${scheduleWhen(p.at)}. Checking tram now, the head will move.`, "good", 8000);
    }
    $("dlg-run").close();
    refreshSoon();
  } catch (err) {
    if (!(err instanceof Gone)) {
      $("run-error").textContent = err.message;
      $("run-error").hidden = false;
    }
  } finally {
    b.classList.remove("working");
    updateRun();
  }
});

$("schedule-new").addEventListener("click", async () => {
  if (!files) await loadFiles();
  openRun("schedule", openFile);
});

async function loadSchedules() {
  try {
    schedules = await api("GET", "/v1/schedule");
    showSchedules();
  } catch (err) {
    if (!(err instanceof Gone)) $("schedule-list").replaceChildren(el("li", "empty", `Can't list them: ${err.message}`));
  }
}

function checkText(label, c) {
  return `${label}: ${c.pass ? "passed" : "failed"}, range ${c.range_mm.toFixed(3)} mm`;
}

let shownSchedules = "";
function showSchedules() {
  const key = JSON.stringify(schedules);
  if (key === shownSchedules) return;
  shownSchedules = key;
  const list = schedules || [];
  if (list.length === 0) {
    $("schedule-list").replaceChildren(el("li", "empty", "No scheduled prints."));
    return;
  }
  // To come first, soonest first, then the ones that have ended, latest first.
  const coming = list.filter((p) => SCHEDULE_PENDING.includes(p.state) || p.state === "printing");
  const ended = list.filter((p) => !coming.includes(p)).reverse();
  $("schedule-list").replaceChildren(...[...coming, ...ended].map((p) => {
    const li = el("li", "");
    const main = el("div", "item-main");
    const text = el("div", "item-text");
    text.append(el("span", "item-name", scheduleWhen(p.at)));
    text.append(el("span", "item-meta", `${p.name}, from the ${p.from === "sd" ? "card" : "Pi"}`));
    if (p.check) text.append(el("span", "item-meta", checkText("Check when scheduling", p.check)));
    if (p.final_check) text.append(el("span", "item-meta", checkText("Check at its time", p.final_check)));
    if (p.reason) text.append(el("span", "item-why" + (SCHEDULE_BAD.includes(p.state) && p.reason !== "cancelled on request" ? " bad" : ""), p.reason));
    const kind = SCHEDULE_BAD.includes(p.state) ? "bad" : p.state === "finished" ? "good" : SCHEDULE_PENDING.includes(p.state) || p.state === "printing" ? "active" : "";
    main.append(text, el("span", "chip " + kind, SCHEDULE_TEXT[p.state] || p.state));
    li.append(main);
    if (SCHEDULE_PENDING.includes(p.state)) {
      const actions = el("div", "item-actions always");
      actions.append(button("Cancel", "danger small", async (b) => {
        const ok = await ask({ title: "Cancel this scheduled print?", text: `${p.name} at ${scheduleWhen(p.at)}.`, yes: "Cancel it", no: "Keep it", danger: true });
        if (ok && await act(b, () => api("POST", `/v1/schedule/${p.id}/cancel`), "Cancelled.")) loadSchedules();
      }));
      li.append(actions);
    }
    return li;
  }));
}

// ---- Printer ----

$("power-on").addEventListener("click", (e) => act(e.currentTarget, () => api("POST", "/v1/power", { on: true }), "The plug is on. The printer connects in about 10 seconds."));
$("power-off").addEventListener("click", async (e) => {
  const b = e.currentTarget;
  const ok = await ask({ title: "Turn the printer off?", text: "The Pi refuses while it's hot or printing.", yes: "Turn off" });
  if (ok) act(b, () => api("POST", "/v1/power", { on: false }), "The plug is off.");
});

$("preheat").addEventListener("click", (e) => {
  const bed = Number($("pre-bed").value);
  const nozzle = Number($("pre-nozzle").value);
  if (!Number.isInteger(bed) || !Number.isInteger(nozzle)) {
    toast("Give whole degrees.", "bad");
    return;
  }
  act(e.currentTarget, () => api("POST", "/v1/preheat", { bed, nozzle }), (out) =>
    `Heating to bed ${out.bed} °C and nozzle ${out.nozzle} °C. They turn off at ${clockTime(out.heaters_off_at)}.`);
});

$("cool").addEventListener("click", async (e) => {
  const b = e.currentTarget;
  if (printBusy()) {
    const ok = await ask({ title: "Cool down and cancel the print?", text: "During a print, cooling down cancels it.", yes: "Cool and cancel", no: "Keep printing", danger: true });
    if (!ok) return;
  }
  act(b, () => api("POST", "/v1/cool"), (out) => out && out.cancelling ? "Cancelling the print, which turns the heaters off." : "Heaters off.");
});

$("tram-start").addEventListener("click", () => {
  $("tram-clear").checked = false;
  $("tram-error").hidden = true;
  $("tram-go").disabled = true;
  $("dlg-tram").showModal();
  $("tram-clear").focus();
});
$("tram-clear").addEventListener("change", () => { $("tram-go").disabled = !$("tram-clear").checked; });
$("tram-go").addEventListener("click", async () => {
  const req = { bed_clear: $("tram-clear").checked };
  const inset = $("tram-inset").value;
  const repeat = Number($("tram-repeat").value || 1);
  if (inset !== "") req.inset = Number(inset);
  req.repeat = repeat;
  const b = $("tram-go");
  b.classList.add("working");
  b.disabled = true;
  try {
    await api("POST", "/v1/tram", req);
    $("dlg-tram").close();
    toast("Checking tram. The head will move.", "good");
    refreshSoon();
  } catch (err) {
    if (!(err instanceof Gone)) {
      $("tram-error").textContent = err.message;
      $("tram-error").hidden = false;
    }
  } finally {
    b.classList.remove("working");
    b.disabled = !$("tram-clear").checked;
  }
});

// ---- More ----

$("hue-pair").addEventListener("click", (e) => act(e.currentTarget, () => api("POST", "/v1/hue/pair"), "Paired with the Hue Bridge."));
$("hue-plugs").addEventListener("click", async (e) => {
  const lights = await act(e.currentTarget, () => api("GET", "/v1/hue/plugs"));
  if (!lights || lights === true) return;
  const list = $("hue-list");
  list.hidden = false;
  if (lights.length === 0) {
    list.replaceChildren(el("li", "empty", "Nothing on the bridge."));
    return;
  }
  list.replaceChildren(...lights.map((l) => {
    const li = el("li", "compact");
    const main = el("div", "item-main");
    const text = el("div", "item-text");
    text.append(el("span", "item-name", l.name), el("span", "mono", `${l.archetype} · ${l.id}`));
    main.append(text, el("span", "chip" + (l.on ? " good" : ""), l.on ? "on" : "off"));
    li.append(main);
    return li;
  }));
});

$("tram-info").addEventListener("click", async (e) => {
  const lines = await act(e.currentTarget, () => api("GET", "/v1/tram/info"));
  if (!lines || lines === true) return;
  $("tram-info-out").textContent = lines.join("\n");
  $("tram-info-out").hidden = false;
});

// ---- Emergency stop ----
//
// Held for a moment, so a stray click can't do it, but with nothing else to get through.

const estop = $("estop");
estop.style.setProperty("--hold", `${ESTOP_HOLD_MS}ms`);
let holdTimer = 0;

function holdStart(e) {
  if (holdTimer) return;
  if (e) e.preventDefault();
  estop.classList.add("holding");
  holdTimer = setTimeout(fireEstop, ESTOP_HOLD_MS);
}

function holdEnd() {
  clearTimeout(holdTimer);
  holdTimer = 0;
  estop.classList.remove("holding");
}

estop.addEventListener("pointerdown", (e) => {
  estop.setPointerCapture(e.pointerId);
  holdStart(e);
});
estop.addEventListener("pointerup", holdEnd);
estop.addEventListener("pointercancel", holdEnd);
estop.addEventListener("lostpointercapture", holdEnd);
estop.addEventListener("keydown", (e) => {
  if ((e.key === "Enter" || e.key === " ") && !e.repeat) holdStart(e);
});
estop.addEventListener("keyup", (e) => {
  if (e.key === "Enter" || e.key === " ") holdEnd();
});
estop.addEventListener("click", (e) => e.preventDefault());

async function fireEstop() {
  holdTimer = 0;
  estop.classList.remove("holding");
  try {
    const out = await api("POST", "/v1/estop");
    const list = [
      out.m112 ? "Stop code M112 sent to the printer." : "No M112 sent, as the printer wasn't connected or was copying.",
      out.plug_off ? "The plug is off, and read back as off." : "The plug is NOT confirmed off.",
      ...(out.problems || []).map(sentence),
    ];
    ask({
      title: out.plug_off ? "Stopped, and the power is off" : "The plug isn't confirmed off",
      text: out.plug_off ? "" : "Use the printer's own switch now.",
      list,
      yes: "OK",
      danger: !out.plug_off,
      cancel: false,
    });
  } catch (err) {
    ask({
      title: "The emergency stop didn't get through",
      text: `Use the printer's own switch now. ${err.message}`,
      yes: "OK",
      danger: true,
      cancel: false,
    });
  }
  refreshSoon();
}

// ---- Start ----

(async () => {
  try {
    info = { ...info, ...(await api("GET", "/gui/info")) };
    $("about-gui").textContent = info.version;
    $("about-address").textContent = info.server;
  } catch { /* shown by the polls */ }
  pollPage();
  pollFull();
  loadFiles();
})();
