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
