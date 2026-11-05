// Tramline's read-only page: the status, with the job drawn by the shared PrintView.
"use strict";

const POLL_MS = 1000;
const $ = (id) => document.getElementById(id);

const view = new PrintView($("view"), (id) => `api/model/${id}`);
if (!view.ok) {
  $("no-webgl").hidden = false;
  $("view").hidden = true;
}

let status = null;

function adjustingText(job) {
  const parts = [];
  const c = job.check;
  if (c && !c.pass) {
    parts.push(`Last tram check: ${c.action} ${c.corner}, turn its knob ${job.turn} seen from above. Range ${c.range_mm.toFixed(3)} mm.`);
  }
  parts.push(job.parked ? "The head is parked. The bed may be hot." : "The head may be low. The bed may be hot.");
  parts.push("Finish with tramline print --continue, or tramline cancel.");
  return parts.join(" ");
}

function addSpan(parent, className, text) {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = text;
  parent.append(span);
}

function showSchedules(list) {
  const items = list || [];
  $("scheduled").hidden = items.length === 0;
  $("scheduled-list").replaceChildren(...items.map((p) => {
    const li = document.createElement("li");
    if (p.ended) li.classList.add("ended");
    // Asking for a cancel isn't a problem, but anything else that stopped it is worth seeing.
    if (SCHEDULE_BAD.includes(p.state) && p.reason !== "cancelled on request") li.classList.add("bad");
    addSpan(li, "when", scheduleWhen(p.at));
    const from = p.from === "sd" ? "card" : "Pi";
    addSpan(li, "what", `${p.name}, from the ${from}, ${SCHEDULE_TEXT[p.state] || p.state}`);
    if (p.reason) addSpan(li, "why", p.reason);
    return li;
  }));
}

function showStatus(st) {
  const job = st.print;
  showSchedules(st.scheduled);
  const badge = $("badge");
  let text, kind, message = "";
  if (!st.connected && !activeJob(job)) {
    [text, kind] = ["printer off", ""];
    message = "The printer isn't connected, or its power is off.";
  } else if (activeJob(job)) {
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "active", ""];
    if (job.state === "adjusting") message = adjustingText(job);
  } else if (st.tramming) {
    [text, kind] = ["checking tram", "active"];
  } else if (st.copy) {
    [text, kind] = ["copying to card", "active"];
    const pct = st.copy.total_bytes > 0 ? Math.floor((100 * st.copy.bytes) / st.copy.total_bytes) : 0;
    message = `${st.copy.name}: ${pct}%`;
  } else if (job) {
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "", ""];
  } else {
    [text, kind] = ["idle", ""];
  }
  badge.textContent = text;
  badge.className = "badge" + (kind ? " " + kind : "");

  $("file").textContent = job ? job.name : "";
  $("nozzle").textContent = st.temps ? temp(st.temps.nozzle, st.temps.nozzle_target) : "–";
  $("bed").textContent = st.temps ? temp(st.temps.bed, st.temps.bed_target) : "–";
  $("version").textContent = `Tramline ${st.version}${st.simulated ? ", simulated printer" : ""}`;

  const notes = [message];
  if (job && job.note) notes.push(job.note);
  if (job && job.stopping) notes.push("Being cancelled.");
  if (job && job.link_lost) notes.push("The link to the printer is lost, so this is out of date.");
  if (job && job.elsewhere) notes.push("Started from the printer, not from here.");
  if (job && view.modelMissing) notes.push("Its file isn't on the Pi, so there's no 3D view.");
  const shown = notes.filter(Boolean).join(" ");
  $("message").textContent = shown;
  $("message").hidden = !shown;
  $("problem").textContent = job && job.error ? job.error : "";
  $("problem").hidden = !(job && job.error);

  const showProgress = job && job.size > 0 && (job.pos > 0 || printing(job)) && !["heating", "soaking", "checking", "adjusting", "starting", "scheduled"].includes(job.state);
  $("progress-block").hidden = !showProgress;
  if (showProgress) {
    const pct = Math.min((100 * job.pos) / job.size, 100);
    $("bar-fill").style.width = `${pct}%`;
    $("percent").textContent = `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
    $("elapsed").textContent = clock(job.run_seconds);
  }
  $("estimate").textContent = printing(job)
    ? job.mode === "stream"
      ? "Head position is worked out from the lines sent, not measured."
      : "Head position is worked out from the card position, read about every 30 s, not measured."
    : "";
  $("estimate").hidden = !printing(job);
}

async function poll() {
  try {
    const resp = await fetch("api/status", { cache: "no-store" });
    if (!resp.ok) throw new Error(resp.statusText);
    const st = await resp.json();
    status = st;
    if (view.ok) view.update(st.print);
    showStatus(st);
    showProgressDetails();
  } catch (err) {
    const badge = $("badge");
    badge.textContent = "can't reach the server";
    badge.className = "badge bad";
  }
  setTimeout(poll, POLL_MS);
}

function showProgressDetails() {
  const d = view.ok ? view.details() : { layer: "–", left: "–" };
  $("layer").textContent = d.layer;
  $("left").textContent = d.left;
}

$("more").addEventListener("click", () => {
  const open = $("panel").classList.toggle("open");
  $("more").textContent = open ? "Less" : "Details";
  $("more").setAttribute("aria-expanded", String(open));
});

poll();
