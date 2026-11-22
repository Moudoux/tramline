# Printing

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Print flow

1. **Check state.** Refuse if a print is running.
2. **Power on.** Skip if already on. Wait for the serial device and an `M115` reply.
3. **Upload.** Skip if the same file is already on the card. This comes before heating, as the user agreed, because a large copy can outlast the 30-minute heater timer, which would stop it. In stream mode there's no copy.
4. **Confirm.** Claude asks the user that the bed is clear, before `print`, as the head crosses the bed during the check.
5. **Preheat.** Bed 80°C, nozzle 150°C, matching `~/Applications/Solidcraft/cura/start_gcode_PETG.txt` so PETG doesn't ooze. The wait at temperature before the first tram check is `tram_soak_seconds`, default 0, as the user's bed is a thin flexible plate made for PETG, not glass, decided 2026-11-03.
6. **Check tram,** in a loop until it passes. See Tram before printing.
7. **Start.** In SD mode, `M23` then `M24`. The start G-code then homes, runs `G29 P1`, and heats the nozzle.
8. **After the print.** Wait for the nozzle to drop below about 50°C, then cut power if requested.

## Print design

**Approved by the user 2026-11-03, and built in `internal/server/print.go` and `internal/marlin/print.go`. Simulator only, and the simulator was written from the same reading of the source, so it can share any misreading.** That includes `cool` cancelling a print, `estop` always cutting the plug, cancelling a print stuck for 30 minutes, refusing files above the temperature limits, and fixing `{machine_depth}`. The findings come from the firmware source at tag `20231202`, read in `dwin.cpp`, `queue.cpp`, `e_parser.cpp`, `cardreader.cpp`, `MarlinCore.cpp`, `M1001.cpp`, `marlinui.cpp` and `hotend_idle.cpp`. **Nothing here has run on the printer.** `libproui` and `prouiex` are closed source, so they can't be read.

| Firmware | Consequence |
|---|---|
| The start-print popup is only reached from the LCD's file menu (`onClickSDItem`, `gotoConfirmToPrint`). `M23` and `M24` don't pass through it | A remote start shouldn't wait at the LCD, and `C10` shouldn't be needed. To test |
| `M23` aborts a running print, and `M27` reports `SD printing byte` for an open file even before `M24` or while paused | Check for a print before `M23`. "Printing" doesn't prove progress, so compare the byte position |
| Commands run in order, so `M27`, `M104` and `M140` wait behind up to 15 queued lines. During `M190` and `M109` no `busy:` is sent, only temperature lines | `M27`'s 3-second limit will fail during a print, marking the link stale, and each resync adds another line to the queue. `cool` can't take effect until the wait ends |
| `M524`, `M112`, `M108` and `M410` are caught as bytes arrive, and a line number before them is allowed, so they can be numbered | They can jump ahead of any waiting command, except during a save |
| `M524` ends `M190` and `M109` waits, then `abortSDPrinting` clears the queue, stops moves, and turns heaters and fans off | Its `ok` can be lost with the queue, so confirm with `M27` and an echoed marker |
| After an abort: `EVENT_GCODE_SD_ABORT "G28XY"` and `M84` | Cancelling moves the head to home at about its current height, raising only to a fixed clearance |
| `M112`: heaters and steppers off, then halted until power is cycled | The next step is the plug. It's parsed only when the board's emergency parser is on, which is off during a save |
| Finishing: `M1001` prints `Done printing file`, and `HAS_LEDS_OFF_FLAG` looks off as no LEDs are configured, so it doesn't wait | The end of a print can be seen on the link |
| `gotoPrintDone` sets `wait_for_user` and a remote start doesn't clear it. `M108` clears it, which should also close the screen | Send `M108` before `M23`, or the next print has `HOTEND_IDLE_TIMEOUT` turned off |
| `HOTEND_IDLE_TIMEOUT`: 10 minutes without extruder movement at 170°C or above sets both heaters to 0, unless moves are queued or `wait_for_user` is set. `M86` reads and sets it, and the printer's saved values may differ | A safeguard for a hung Pi during streaming, already in the firmware |
| `M85 S<seconds>`: kills the printer after that long with no moves queued. Heat waits and moves reset it. Off by default, not saved | A faster safeguard for a hung Pi during streaming: set it at the start, and clear it at the end |
| `POWER_LOSS_RECOVERY` is built in, off by default, and `M413` reads it | If it's on, a plug cut mid-print may bring up a resume popup at next power-on. To check |

**Also found in the existing files:**

- An SD print runs whatever temperatures the file says, outside the server's limits. So `gcode.Measure` now refuses `M104` or `M109` above 260°C, and `M140` or `M190` above 100°C, at upload.
- CuraEngine CLI files had an unresolved `G1 X0 Y{machine_depth}` in their end G-code. It's expected to have moved X only, so the print wasn't presented, but that's from memory of the parser, which hasn't been read. `~/Applications/Solidcraft/cura/resolve_settings.py` now writes Y220, as the setting's own value of 235 is past this printer's Y limit of 231.

**Design:**

- **Job:** `PrintJob` beside `CopyJob`, with a `Mode` of `sd` or later `stream`, saved to `/var/lib/tramline/print.json`. `tramline print <file> --bed-clear` needs a verified copy on the card with the right `M20` size. It sends `M108`, `M23` with the size checked, `M24`, then `M27`.
- **Lock:** a print doesn't hold the operation lock, as tram doesn't between steps. A `printRunning()` check refuses `tram`, `preheat`, `copy`, `tram info`, `files rm` and `power off`, without sending `M27`.
- **Polling:** a print goroutine holds one `M27` at a time, outside the lock, every 30 seconds. It waits as long as temperature lines keep arriving, fails after 10 seconds of total silence, and gives up after 15 minutes.
- **Idle timer:** restarted only when the byte position has moved, so a paused or stuck print still ends after 30 minutes, with `M524`. A running print counts from `M24`, closing the 30-second gap in the plug cut.
- **`cool` during a print:** becomes a cancel. Heaters off with the print still moving is the worse result.
- **`cancel`:** numbered `M524` written straight to the port, then confirmed.
- **`estop`:** takes no lock. During a copy, the plug only, as `M112` would be ignored. Otherwise `M112` then the plug straight away, without waiting on each other. With no link, the plug only. The hotend fan stops too, which the safety rules accept for an emergency stop.
- **Numbering:** the link owns one line counter, started with `N0 M110 N0` by the first command after the handshake. Each write takes its number under a short write lock, which `Urgent` also uses to get past a waiting command. An `ok N<line>` for an earlier line isn't taken as the reply, and `Resend` re-sends from a history. A copy numbers its own lines from 0, and the count starts again after it. **Not numbered,** as the firmware's count isn't known at those moments: `M115` and `M29` in the handshake, the blank line that clears garbage, and the `M29` and marker that end a save after a failure. A marker the firmware refuses is what proves a save is still open.
- **Unprompted lines:** a callback passes `Done printing file` and `Error:` halts to the job.
- **Restart:** at connect, `M27` and `M27 C` adopt a running print, including one started from the LCD. The handshake must stop re-sending `M115` while temperatures are arriving, or `M190` would fill the queue and drop the link after 30 seconds.
- **Link drop:** the job shows "link lost, print presumed running", then "ended while disconnected, outcome unknown" if `M27` later says so. If the plug reads off, the job fails with power lost.
- **Streaming, stage 10:** done, see Streaming design.

**Simulator tests:** finishing, every refusal, a wrong size on the card, cancelling, including during a heater wait with `M27` stuck behind it and `M524`'s `ok` lost, `cool` as a cancel, a stuck print, `estop` with and without a working plug and during a copy, a restart with the print running and after it ended, prints started and stopped from the LCD, a lost link, the plug off, a firmware halt, numbering, resends, and the CLI's calls end to end.

**Read-only test on the real printer,** 2026-11-03 at 19:27 to 19:33 GMT, with `dev-20261103192526`. Logs are in `test-logs/2026-11-03-print/t1-info/`.

- **Numbered commands work:** `tram info` replied `ok N4 P15 B15` to `ok N10 P15 B15` with no resends or errors.
- **Power-loss recovery is off:** `M413 S0`, so a plug cut shouldn't bring up a resume popup.
- **Hotend idle timeout is on:** `M86 B0 E0 S600 T170`, matching the source: 10 minutes, 170°C and above, both heaters to 0.
- **Connected** about 11 seconds after `power on`, with temperatures. `power off`: USB dropped about 2.5 seconds later.
- **Pi watch:** 70 checks over 5 minutes of ping, login, `sync`, throttling and failed units, none bad.

**Cold print tests on the real printer,** 2026-11-03 at 19:34 to 19:44 GMT, with `dev-20261103192526`, using `dry-run.gcode`: no heat, no G-code movement. Logs are in `test-logs/2026-11-03-print/`. These settle Open question 2.

| Test | Result |
|---|---|
| Remote start | **No LCD confirmation:** it went straight to the print screen, reported by the user, so `C10` isn't needed |
| To the end | About 61 seconds. `Done printing file` arrived, and the job finished straight away. The LCD shows print done with a Continue button, which doesn't block anything |
| `M27` while printing | First reply took about 16 to 18 seconds, behind the queue. `M108` then `M24` worked, 0 resends |
| Starting with the print-done screen up | It closed, reported by the user |
| Cancel after 15 seconds | About 10 seconds to confirm. `//action:cancel`, `Print Aborted`, then homed to `X:0 Y:0 Z:5`, with heaters still 0. LCD shows print aborted with a Continue button. `M524`'s `ok` did arrive, so losing it is simulator only |
| `estop` after 15 seconds | `M112 Shutdown` and `Printer halted` about 20 ms after the request, plug read off 0.33 seconds later, USB dropped 2.9 seconds after the request. 0.4 seconds for the whole command |
| Pi watch after `estop` | 70 checks over 5 minutes, none bad |
| Power on after `estop` | Connected in 11 seconds. The card lists normally |

**Server restart during a cold print,** 2026-11-03 at 19:45 to 19:53 GMT, with `dry-run-long.gcode`. Logs are in `test-logs/2026-11-03-print/t5-restart/`.

- **The print and board survived both restarts:** no `start` line, messages carried on, the LCD didn't change. Reconnected about 15 seconds after the restart, as `M115` waits behind the queue.
- **The first restart failed to pick it up,** with `dev-20261103192526`: a 30-second limit on asking kept running out behind the queue, and each retry queued more. Fixed in `dev-20261103194900`, which asks once and waits, without holding anything else up. The simulator had missed this, as its queue answered too quickly.
- **The second restart passed,** with `dev-20261103194900`: picked up at 69% about 64 seconds after the restart, then cancelled.

**First real print,** 2026-11-03 at 19:54 to 20:04 GMT, with `dev-20261103194900`, `test-tile_PETG.gcode`. Logs are in `test-logs/2026-11-03-print/t6-tile/`. How the part came out is for the user to report.

| Stage | Time |
|---|---|
| Copy | 2.2 s, 1020 lines, 13.9 KB/s, 0 resends |
| Bed from 23 to 80°C, homing, `G29 P1`, `C108`, nozzle to 235°C | 7 min 11 s, from 19:54:24 to about 20:01:35 |
| Printing | 2 min 45 s, matching the slicer's estimate of 165 s |
| Total | 9 min 56 s |

- **No stall** at `G29 P1` or `C108`, and no resends.
- **`M27` waited through it all:** the first one was sent at the start and answered about 7 minutes later, with temperatures still arriving. The old 3-second limit couldn't have worked. Once printing, position moved every 30 seconds or so.
- **Targets followed the file:** 150, 235, then 230°C, and bed 80, then 75°C, then both 0. `Done printing file` came as the heaters went off, and the job finished at once.
- **`M84 X Y E`** answered `X not disabled. Shared with Z.`, and likewise for Y and E0, so on this printer it leaves them on until the 120-second stepper timeout. Harmless.

**Cancelling a real print,** 2026-11-03 at 20:10 to 20:16 GMT, with the same tile, in the first layer with the nozzle at 235°C. Logs are in `test-logs/2026-11-03-print/t7-tile-cancel/`.

- **Targets read 0** in the next status, about 5 seconds after the request. The cancel was confirmed 7.4 seconds after it, homed to `X:0 Y:0 Z:5.90`.
- **Stage 9 passed.** Reported by the user for the first print: adhesion good, no stringing, not foamy, as good as manual prints. The LCD showed the file name, and the mesh viewer opened and closed.
- **Heat from a warm bed:** 5 min 10 s from `print` to printing, bed starting at 52°C.

**User's requests after this test** are built, see Tram before printing.

**Tests, each asked for separately:** read-only `M413`, `M86`. A cold file of `G4` and `M118` lines, to finish, to cancel, to `estop`, and across a server restart. Then a small real print to finish and to cancel, at the profile's temperatures: bed 80 then 75°C, nozzle 150, 235, then 230°C.
