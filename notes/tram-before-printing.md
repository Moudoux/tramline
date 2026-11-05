# Tram before printing

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

**Asked for by the user after stage 9: always check tram before a print, in a loop, with no override, and show the steps on the LCD. Design approved and built 2026-11-04 in `internal/server/prepare.go`. Only the pass path has run on the printer, see First real run and Stand print result.**

`POST /v1/print` makes its refusals and its card check, then returns straight away with the job in `heating`. The job goes `heating`, `soaking`, `checking`, then on a pass `starting` and `running`, or on a fail `adjusting`, where nothing runs until `tramline print --continue`, which goes back to `heating`. The limit is the wizard's 0.05 mm.

| State | Heaters | Head | Server restarts | `cancel` or `cool` |
|---|---|---|---|---|
| `heating`, `soaking` | On, 80/150 | Wherever it was | Becomes `adjusting`, with a note | Immediate: heaters off |
| `checking` | On | Moving | Becomes `adjusting`, "may be low" | Finishes the current `G28` or `G30`, parks, heaters off |
| `adjusting` | Held until the timer runs out, then off, and the job keeps waiting | Parked at X115 Y115 Z30 | Stays `adjusting` | Immediate: heaters off, head stays parked |
| `starting` | On | Parked | If `M27` shows this file printing, adopted as `running`. Otherwise `adjusting` | Waits for the start, then a normal `M524` cancel |

- **Heating:** `M140 S80` and `M104 S150`, then the temperature reports must stay within 1°C of both for 10 seconds, failing after 15 minutes. It doesn't use `M190`, so the link stays free.
- **Check:** one normal tram run at inset 25, so it's in `status` and the history. It always homes first, as the steppers time out during a wait and the user may have probed from the LCD. It parks on a pass too, as the file might fail to start.
- **Wizard during a wait:** the user may run the LCD's tram wizard while `adjusting` until it's green, then ask for the final check. Only Tramline's check starts the file. No code is needed for this, and it hasn't been tried.
- **Soak:** `tram_soak_seconds`, 0 to 1200, so it can't use up the heater timer. It runs before the first check, and again if `--continue` finds the heaters were off. Other rechecks skip it. All the hot readings so far followed a 5-minute soak, so a soak of 0 hasn't been tried.
- **Heater timer:** the idle timer keeps running through all these states, and `--continue` restarts it, as `preheat` does, so a check can't be cut short, decided by the user. A timer that runs out leaves the job waiting with the heaters off, and `--continue` heats again.
- **`--continue`:** also counts as saying the bed is clear, decided by the user, as they're at the printer. It's refused unless a job is `adjusting`, and with the plug off or no link, leaving the job waiting.
- **Targets changed** by the LCD or the timer during a check or heating: the job goes to `adjusting`, parked if it was checking.
- **Failures:** a failed command or a lost link ends the job as `failed`, not parked, heaters off if it can reach them, or else the idle timer and plug cut apply as usual. A firmware halt ends the job in any state.
- **Other commands:** `print`, `tram`, `preheat`, `copy`, `files rm` and `power off` are refused until the job ends. `estop` still acts at once. A print started from the LCD closes the job, and adoption follows that print.
- **Temperatures:** fixed at 80/150, matching the PETG start G-code, decided by the user. Another material or bed target would need this changed.
- **Knob direction:** `status` gives it as seen from above, lower counter-clockwise, raise clockwise, measured only on back-left.

**LCD, from reading the source, so it may differ, as `libproui` is closed:** `M117` only sets the status line at the bottom, and the title of the print screen is fixed to "Printing" by `drawPrintProcess`. So before `M24`, `M108` closes the print-done screen and `M117` shows `Heating bed`, `Checking tram`, for example `Lower back-left CCW`, and `Starting print`. A plain `M117` clears it if the job ends early. `M117` must come after `M140` and `M104`, as each writes its own message and sets a function that resets the line at temperature (`set_heating_message`, `M140_M190.cpp`), and `M117` cancels that function. After `M24` the firmware writes its own status: `M190` "Bed Heating", which is skipped when already hot, `G29`'s own leveling screen and a probing progress line, `M109` "Heating...". `M117 Heating nozzle` was added to the start G-code just before `M109`. By the source `M109` should replace it straight away, but on the stand the user reports it stayed up during the heat, so the source reading is wrong here. The title will still say "Printing" during the mesh and nozzle heat, and `M117` can't change that.

**First real run,** 2026-11-04 at 10:58 to 11:04 GMT, with `dev-20261104105333` and `dry-run.gcode`, from cold. Logs are in `test-logs/2026-11-04-tram-loop/`. Only the pass path has run on the printer. A failed check, `--continue`, the wizard during a wait, a soak above 0, and every failure and restart path are still simulator only.

- **Times:** heating 4 min 19 s from 22°C, check 39 s including 5 s to park, file started 40 ms after parking, file 60 s.
- **Check with no soak:** range 0.035 mm, pass, from average +0.011, -0.011, -0.018, +0.017. Very close to the soaked five-round averages of 2026-11-03, on one run.
- **No errors, resends or stale link** in the server's log.
- **LCD, reported by the user:** "Nozzle heating", then "Ender3V2 ready" while the bed was still heating, as `M117` went before `M140` and `M104`. During the check it showed the bed's coordinates at each corner, which is `G30` replacing `Checking tram`.

**Stand print start,** 2026-11-04 at 11:13 to 11:21 GMT, with `dev-20261104110522`, from a warm bed. Logs are in `test-logs/2026-11-04-stand/`.

- **LCD fix confirmed by the user:** `Heating bed` stayed up while heating, and `Checking tram` showed.
- **Times:** heating 3 min 24 s from bed 40°C, check 41 s, file started 40 ms after parking.
- **Check:** range 0.040 mm, pass, from average +0.016, -0.013, -0.021, +0.019.
- **Copy:** 92,794 lines in 217.6 s, 12.7 KB/s, size checked, 0 timeouts, but 16 resends, all `Line Number is not Last Line Number+1`, so whole lines seem to have been lost on the way, about 1 in 5,800. The resends worked. The cause is unknown, and it matters for streaming, where a resend happens during the print. See Lost lines.

**Stand print result,** 2026-11-04, print job 8, with `dev-20261104110522`. Logs are `server.log`, `kernel.log` and `power-off-kernel.log` in `test-logs/2026-11-04-stand/`.

- **Outcome:** `finished`, with 2,756,863 of 2,756,863 bytes read and `Done printing file` at 15:31:37 GMT. The user reports the part came out mostly good, with minor stringing at the back that they think comes from the model, not Tramline. From a photo, wisps run along the waist above the base and across an inside corner, which looks like PETG stringing on travel moves. Damaged data is unlikely: lines are checksummed and the size matched, though the card's copy hasn't been compared byte for byte.
- **Time:** 4 h 10 min 34 s from `M24`, 4 h 10 min 17 s by the firmware, against the slicer's 4 h 19 min 37 s. That's about 9 minutes shorter, even with about 2 min 50 s of mesh and heating that the slicer doesn't count. Timed from the end of the prime line, the print ran at 0.96 of the slicer's time at line 84231 and 0.95 at the end, so the speed-up was steady.
- **Idle timer:** the first expiry was 11:46:58, and the print ran 3 h 45 min past it with no `heaters off, idle timeout` and no `hasn't moved on`. This rests on those lines being absent, the job finishing and the part being good, as temperatures aren't logged.
- **Link and firmware:** no link loss, no firmware halt, no `couldn't read its position`, no service restart. The Pi didn't reboot, and the kernel log has no entries from 11:13 until the power off.
- **Limit of "no firmware error":** only halts are always caught. Lines arriving while an `M27` waits are thrown away, so only 3 of the file's 6 `G92` position reports were logged. A non-fatal message could have been missed the same way.
- **First `M27`:** its wait wasn't logged and `status.log` stops at 11:21:03. It took at most about 2 min 51 s, as position lines logged at 11:23:53 only appear with no command waiting. The two `G92` reports just before them were lost, so an `M27` was waiting at that point, though it may not have been the first.
- **Position lines:** Marlin reports its position on `G92`, so `X: Y: Z:` lines in the log are normal. The one at 15:11:34 matches line 84231 of the file exactly, at X75.28 Y103.15 Z24.68.
- **LCD, reported by the user:** `Heating nozzle` stayed up during the heat, and the print-done screen came up.
- **End of print:** the bed came forward, the first real use of the `Y220` fix.
- **Cooling:** the nozzle was at 31°C and the bed at 51°C 7.5 minutes after the end, and 25.8°C and 43°C after about 12 minutes.
- **Power off:** `power off` at 15:43:19, with the kernel log captured. A clean USB disconnect at 15:43:22, with two `urb stopped: -32` lines just before it, and no errors after. The Pi was fine about 70 seconds later, which is a short watch compared with the earlier hang's timing.

**Lost lines: offline investigation,** 2026-11-04, from existing logs, the file and source reading, with no printer. The cause is still unknown. The scripts were throwaway, and the numbers come from `test-logs/2026-11-04-stand/server.log`, `test-logs/2026-11-03-sd-window/server.log` and the G-code.

- **Rate:** about 17 events in about 112,000 lines at window 120. The stand's rate may not be worse than earlier copies: the 15,264-line file would expect 2.6 at 1 in 5,800, and seeing 1 or fewer has a 26% chance. The rate at one line at a time isn't known, as only 2,000 lines have run that way, too few to expect one.
- **What the error rules out:** the firmware checks a line's number before its checksum, in `get_serial_commands`, and a damaged line or a lost newline would mostly fail the checksum. All 16 of the stand's errors were line numbers, so random byte damage on the wire doesn't fit, as only about 1 byte in 6 is in the number. Test A then gave 4 checksum errors in 17, see below, so partial damage happens too, but line-number errors still dominate, at 29 of 33. A lost first byte would give a missing-number error, not seen.
- **Tramline's code, by reading only:** `sendLines` sends in strict order, with each line in one write, from one goroutine. The only other writers are `M524` and `M112`, which didn't run, `M29`, and a newline at connect. No path that skips a line was found. A kernel or USB loss can't be seen from here, and the kernel log shows no errors.
- **Same lines each time?** Unknown. It needs a repeat copy.
- **8 KB card boundaries:** at windows 240 and 480, 14 of 18 resends were at 86 to 114 bytes past an 8,192-byte boundary of the file, so that overflow is closely tied to the board's stall. At window 120 it isn't: the stand's events spread across the cycle, the earlier event was at 137 bytes past one, and the stand's lean towards the boundary is weak (p about 0.05 for that one period, nothing after allowing for trying many periods). So the 120-byte losses are probably not that overflow, though some card effect isn't ruled out.
- **Line shape:** lost lines are more often `G0` moves (11 of 16 against 42% of the file, p about 0.03) and shorter (mean 35.8 bytes against 38.8, p about 0.03, partly the same effect). This wasn't corrected for trying several tests, so treat it as a lead. A short line is more likely to fit at the end of the window, so the board's buffer may matter.
- **Timing:** irregular, 0.6 to 23.5 seconds apart. The events do not fall at the `M155 S2` temperature reports, which come at about 1.48 s in a 2-second cycle.
- **What it means for streaming:** a stream would run far slower than a copy, about 6 lines a second on average on the stand, against 427 in the copy, and with one or a few lines unanswered. So the copy's rate may not carry over. All 35 real resends have recovered. The risk is the pause: `sdSettle` is 200 ms, while the planner's 15 moves could last much less on short segments. The 200 ms needs revisiting for a stream.
- **CH340 packet size:** `/sys` shows 32 bytes on the bulk OUT endpoint (`ep_02`, read 2026-11-04). So a single lost USB packet can't remove a 41-byte line, though more than one could.

**Lost lines: test A,** 2026-11-04 at 16:24 to 16:27 GMT, with `dev-20261104110522`, cold, no heat or movement. Logs are in `test-logs/2026-11-04-lost-lines/a-window120/`. An identical copy of the stand's file, by `cmp`, went to the card as `lost-lines-test.gcode`, `LOSTLI58.GCO`, at window 120, using a new name because `files rm` also deletes the Pi's copy. Both are still there.

- **Result:** 92,794 lines in 215.6 s, size checked, 0 timeouts, 17 resends against the stand's 16. The rate repeats closely: about 1 in 5,500 to 5,800.
- **Not the file's content or position:** none of the 17 lines matches the stand's 16, the nearest being 167 lines away. So the cause is timing or something else that changes between runs, on a fixed numbering and fixed bytes. The only other difference is the file's place on the card.
- **Errors:** 13 line-number and 4 checksum (at lines 889, 35281, 52456, 68578), where the stand had 16 and 0. The kernel log shows no USB errors.
- **The earlier leads didn't repeat:** `G0` lines were 8 of 17, about the file's 42%, or 19 of 33 combined (p about 0.06). Test A's positions show no lean towards 8 KB boundaries.
- **Reasoning, not measured:** a pause in the board taking in bytes, starting between lines and lasting at least one line (1.6 ms at 40 bytes), could give mostly whole-line losses and a few partial ones. Firmware that's partly closed can't confirm this.

**Lost lines: test B,** 2026-11-04 at 16:45:39 to 16:53:05 GMT, cold, no heat or movement. Logs are in `test-logs/2026-11-04-lost-lines/b-window1/`. An identical file, as `lost-lines-test-b.gcode`, `LOSTLID6.GCO`, at window 1, one line at a time.

- **Result:** 92,794 lines in 446.2 s, 6.2 KB/s, 208 lines a second, size checked, **0 resends and 0 timeouts**, where window 120 gave 16 and 17. That can't be chance. The kernel log shows nothing new.
- **Not a steady rate over time:** it ran twice as long, so a loss per second would give about 33.
- **So the losses need lines overlapping on the way to the board.** At window 1, no bytes arrive while the board handles a line. That also counts against test A's idea of a pause just after an `ok`, as that would have lost lines here too, showing as timeouts.
- **The cause itself is still unknown:** bytes arriving during a card write or a fuller buffer both fit, and this can't tell them apart.
- **Speed needed by a stream:** the stand averages about 6 lines a second, at most 10.2 in any layer by the slicer's times, and at most about 81 in any 1 second by a rough estimate from move lengths, ignoring acceleration, which overstates it. Window 1 managed 208 here, with card writes.
- **Limits:** one run, cold, with no moves and with card writes, which isn't the same as a stream during a print.

**Remaining tests, if wanted, each needing the user's OK, cold:** window 80, mostly 2 lines, as window 60 would rarely fit a second line. Numbered no-ops with no card write, which needs a small build. Neither is needed if the stream sends one line at a time.

Both test files were removed from the Pi and the card afterwards, and the user agreed to stop the printer tests here.

**Simulator tests:** `prepare_test.go`, covering the order of commands, a failed check then a recheck then a pass, refusals while waiting, cancel and `cool` in each state, `estop` waiting and during a check, a failed probe, a firmware halt, targets changed, the timer running out then `--continue`, soaking, restarts in each state, an LCD print during a wait and a lost link. All tests were written with the code, and the simulator comes from the same reading of the source.
