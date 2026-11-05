# Scheduled prints

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

**Asked for by the user 2026-11-05, built in `internal/server/schedule.go`. Tests and the simulator only so far.**

| Step | What happens |
|---|---|
| `schedule` | Refuses a time under 15 minutes or over 30 days away, or a missing file. Switches the plug on if needed, waits for the link, then starts a normal print job marked `pre_check` |
| First check | The same `heating`, `soaking`, `checking` and `adjusting` as any print, with `print --continue` and `cancel`. The schedule is `checking` |
| A pass | The job ends as `scheduled`, heaters off, the schedule becomes `waiting`, and auto-off cuts the plug once the nozzle is cool |
| Its time | Switches the plug on, waits up to 90 seconds for the link, and starts a normal print job. The schedule is `starting`, then `printing` |
| Second check, pass | The file starts, by the card or the stream as usual |
| Second check, fail | Cancelled at once, decided by the user, with no recheck and no `adjusting`, as nobody is there. Heaters off, auto-off |
| The print ends | Any way it ends, auto-off runs, decided by the user. Every print now does this, see CLI changes below |

Decisions:

- **Storage:** `schedules.json` in the data folder, written with `writeAtomic` like `print.json`, as there are only a handful of records. SQLite would need cgo or a large third-party module, against the standard-library rule. The 20 newest ended records are kept. If the file can't be read, `schedule` and `schedule list` refuse rather than overwriting it.
- **Time:** the client reads a full date in its own timezone and sends RFC 3339, so the Pi being on GMT can't shift it. A date is required, and "6 am tomorrow" is for the skill to turn into one. The server looks at the wall clock every 5 seconds, not a timer set in advance, as the Pi has no battery-backed clock. Its clock runs behind after a boot until it syncs, which can only make a print late, not early.
- **Bed clear, decided by the user:** several can wait, but each confirmation only holds until a print starts. `bed_used_at` is saved whenever a file starts, from here or found running from the LCD, and a schedule whose `bed_clear_at` is older cancels itself without touching the printer. So two scheduled prints only both run if the second was scheduled after the first's part came off. It's cancelled, not held, as nobody can say the bed is clear at that moment.
- **Late, decided by Claude as a default:** past its time by over 15 minutes, because the server was down, it's `missed` and doesn't start. A first check that hasn't passed by the scheduled time is cancelled.
- **Busy, decided by Claude as a default:** anything using the printer at that time, including a job waiting in `adjusting`, cancels the schedule and leaves that job alone.
- **Files:** `files rm` and uploading over a file are refused while a schedule that hasn't started printing needs it.
- **Auto-off is stricter than `power off`:** it needs a connected printer with known temperatures, targets at 0 and the nozzle below 50°C, as `cutPowerBlocker` lets a lost link through. It turns the heaters off once at the start, for a file with no end G-code. After that, a target above 0, or any job, means someone is using the printer, and it stops and leaves the plug on. So does `power on`, or scheduling or firing another. If it can't confirm cool within 30 minutes it gives up with the plug on, so the fan keeps running. `off_pending` is saved, so it carries on after a restart.
- **Restarts:** a waiting schedule simply carries on. The first check goes to `adjusting` with the usual note. A check at the scheduled time also goes to `adjusting` as usual, so `adoptPrint` can follow a file that did start, then once the printer has answered it's cancelled, with nobody there to continue it. A stream cut off by a restart is failed as usual, which the schedule picks up on its next look.
- **Nothing tells the user** when one is cancelled or fails: `schedule list` and the log are the only record, and the printer will be off.
- **Web page:** only knows the `scheduled` job state. It doesn't list scheduled prints.

**CLI changes, 2026-11-05, asked for by the user:**

- **Auto-off is on every print** Tramline starts, however it ended, including a `cancel` while `adjusting`, not just scheduled ones. The user's words were "all scheduled prints, not just scheduled", taken to mean every print. It doesn't apply to prints from the LCD, or a tram check when scheduling that was cancelled or failed. When another print has taken over a job, closing it doesn't start one (`closeJobPower`), as its first step turns the heaters off. Its saved state stays in `schedules.json`.
- **`print` switches the plug on,** as `schedule` does, after refusals that don't need the printer, so a mistake doesn't power it up.
- **`upload` only saves on the Pi,** and `--sd`, `--pi-only`, `--no-wait` and `--window` are gone from it. `copy` is the only route to the card.
- **`mode` and `settings.json` are gone,** with `GET` and `PUT /v1/settings` and `FilesResponse.Mode`. An old `settings.json` on the Pi is ignored. Earlier sections that mention `mode auto` describe what was tested then.
- **`--from pi|sd` is required** on `print` and `schedule`, and `from` in the API, and it's stored on a schedule. Neither falls back: `sd` is refused with no verified copy, and `pi` streams even with one, which ends the "new name with `--pi-only`" workaround. The job's own `mode` is still `stream` or `sd`.
- **`<file>`** replaces `<name>` in the help, meaning the file's name on the Pi. Commands that take it still accept a path and use its last part.
- **Web page:** `WebStatus.Scheduled` lists all that haven't ended, soonest first, then the last 5 ended, with the reason. Times show in the browser's timezone. The page has no login, so this newly shows those names and times to anyone on the LAN or tailnet. `sim-web.sh --schedule <minutes>` tries it.

**Simulator run of these changes,** 2026-11-05, with the real binaries. `print --from pi` from a plug that was off took 5 seconds to return, finished at 19:34:37, and the plug went off at 19:34:42. The nozzle had read 126°C just before. `--from sd` with no copy and a missing `--from` were refused. The page was checked in headless Firefox with one waiting and one cancelled schedule, without WebGL, on `dev-20261105182938`. The two later small changes to the page, no red for a requested cancel and no bar for a job that never printed, weren't looked at again. A `--from sd` print through the whole flow ran in the tests, not this run.

**Tests:** `schedule_test.go` has refusals, the whole flow, adjusting at the first check, a failed second check, the bed guard with one and two schedules, a busy printer, cancelling in each state, file protection, missed and within the grace, a plug failure, restarts in each state, auto-off's rule, and one with a simulated printer that really goes away when its plug is off.

**Simulator run with the real binaries and clock,** 2026-11-05, 18:56 to 19:13 CET, with `dev-20261105175455`, so a client on CET against a server process using the same machine's clock. The file streamed from the Pi side, and the plug started off. The time was entered as 19:13 CET and stored as 18:13 UTC. The refusals worked, the first check passed and the plug went off, and `files rm` was refused. A second schedule was made and cancelled during its check. The server was restarted while the first waited, and it kept it. The heaters came on at 19:13:05, the file started after its second check at 19:13:22, and the plug went off at 19:13:27, after `status` had shown the nozzle at 95°C at 19:13:23. The log wasn't kept, as it was in a scratch folder. The only bug it found was a `status` line left showing after a cancelled check, since fixed in `dev-20261105181359`. A card print and a failed second check ran in the tests, not this run. The web page's one new line hasn't been seen in a browser.

**Not proven on the real printer:** all of it. Worth testing first, each with the user's OK, with a cold or tiny file at a time 15 to 20 minutes away:

1. Real Hue plug and reconnect at the scheduled time, which has only had the simulated plug. The repeat power cycles matter here because of the Pi hang and the plug's self-off, so capture the kernel log.
2. Whether a board powered on by the plug behaves the same at 05:59 with nobody watching. The user still needs to review Hue rooms and routines before relying on this overnight, as a routine could switch the plug.
3. A failed second check, by turning a knob during the wait, and auto-off from a truly hot nozzle: the simulator moves temperatures at 40°C a second, so the wait for below 50°C has only lasted seconds.
