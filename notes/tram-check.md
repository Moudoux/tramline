# Tram check

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

**This matches the firmware wizard, as the user asked.** The rule below is what the LCD uses, with no custom thresholds:

1. Probe the four corners with `G30`, in the wizard's order: front-left, front-right, back-right, back-left.
2. Subtract the average from each reading.
3. **Pass if the highest and lowest differ by less than 0.05 mm.**
4. Otherwise, name the corner furthest from the average: "Lower" if above it, "Raise" if below.

`G30` is used because the wizard reports to the LCD only, but the rule and output are the same.

**Built 2026-11-03 in `internal/server/tram.go` and `internal/marlin/motion.go`. It has only run on the simulator and fakes.**

- **Run:** `G28`, then `G30` at each corner, then, if all went well, `G90`, raise to Z30, move to X115 Y115, `M400`. The request returns straight away, the run carries on if the client goes away, and `status` shows it. Each `G30` reply's X/Y must match what was asked for, within 0.1 mm.
- **Refused:** without `bed_clear`, with the plug off or unknown, not connected, temperatures unknown, an SD print running or unknown, or a run already going.
- **Limits:** silence 60 seconds, `G28` 3 minutes, `G30` 90 seconds, park commands 1 minute each. These are generous until there's real data, and the server logs each command's time and longest silence. Temperature reports don't count as activity.
- **Operation lock:** held per step, so `cool` and the idle check get a turn between steps. The firmware can't act on them during a move anyway, so `cool` can wait as long as that move's limit, and the client allows for that. `preheat` and `power off` are refused straight away during a run, so **the printer's own switch is the only stop until `estop` exists**.
- **Idle timer:** tramming doesn't extend it. If the heater targets change during a run, by the timer, `cool` or the LCD, it stops with no verdict and parks. A stuck step can hold up the idle check by up to 3 minutes, and then the existing plug cut logic applies unchanged.
- **Failures:** any failed command, or a dropped link, ends the run with no verdict and no parking, as the head's state isn't known. Readings so far stay in `status`. There's no automatic resume or power change. `Error:Printer halted` fails at once.
- **Late replies:** after a timeout, the old command's `ok` could be taken for the next command's. The link now marks itself stale, and sends `M118 sync-<n>` before the next command, discarding everything up to that echo. While the old move is still running, later commands fail with "still busy". This covers heating commands too.
- **Levelling:** `G30` leaves bed levelling off. The start G-code turns it back on with `G29 A`.
- **History:** every finished run is appended to `/var/lib/tramline/tram-history.jsonl`, because the Pi's journal doesn't survive a reboot.
- **Known gap:** tramming commands have no line numbers or checksums, as only transfers to the card do. The X/Y check only catches a corrupted `G30` after it has moved, and `G28` and park have no such check.

**`tram info` on the real printer,** 2026-11-03 at 16:33 to 16:39 GMT, with logs in `test-logs/2026-11-03-tram-info/`. It's the first real-printer run of any of this code, and nothing moved.

| Setting | Value | Meaning |
|---|---|---|
| `M118 tramline-check` | Echoed exactly, then `ok` | The resync's marker works on this board. The resync itself hasn't run here, as no command has timed out |
| `M851` | X-41.50 Y-7.00 Z-0.90 | Probe offset. From this, the probe should reach X up to 206.5, so 200 and 205 both fit |
| `C29` | L25 R205 F25 B205, 5x5, T=50 | Mesh limits, shown in `M503`. So 25/205 is the mesh, but the wizard may still use 30/200 |
| `C851` | S480 M0 | Probe Z speed 8 mm/s, multiple probing off |
| `M503` other | `M420 S0` (levelling off at boot), `M203 Z10`, `C125 X240 Y221 Z20` | Z limited to 10 mm/s, and the firmware's own park position |
| `M114` | All zero | Not homed since power-on |
| `G29 W` | Only `ok` | Printed nothing on this build, unlike the source read, so it's no use |

Connected about 10 seconds after `power on`, and `tram info` took about 1 second. USB dropped about 2 seconds after `power off`. The Pi watch ran 57 checks over 5 minutes, none bad, and the kernel log showed only the usual `urb stopped: -32` twice before the disconnect.

**First real `tram` run,** 2026-11-03 at 16:41 GMT, cold (bed 23.5°C, nozzle 22.6°C), with the user at the printer's switch. Logs are in `test-logs/2026-11-03-tram-first/`. One run only, so nothing here shows repeatability yet, and no knob has been changed.

| Corner | X, Y | Z | From average |
|---|---|---|---|
| Front-left | 30, 30 | +0.045 | +0.005 |
| Front-right | 200, 30 | +0.005 | -0.035 |
| Back-right | 200, 200 | +0.015 | -0.025 |
| Back-left | 30, 200 | +0.093 | +0.053 |

Range 0.088 mm against the 0.05 mm limit, so it failed, naming "lower back-left". Not yet compared with the wizard.

| Step | Took | Longest silence |
|---|---|---|
| `G28` | 19.53 s | 2.77 s |
| `G30`, each of 4 | 4.83 to 5.41 s | 2.19 to 2.52 s |
| Park, nearly all in `M400` | 5.03 s | 2 s |
| Whole run | About 45 s | |

So `busy:` does arrive about every 2 seconds on the real board, and `G30`'s reply parses as expected. The limits have lots of room, and are left alone until there's more data.

**Startup, same power-on:** the server's log now shows what the early `ok` follows: the board's settings and SD messages, ending with the baud rate line. It also printed `//action:notification Stored settings retrieved`, so some status messages do reach serial, despite `HOST_STATUS_NOTIFICATIONS` being off in the config read. `External Reset` appeared again.

**LCD wizard, compared,** 2026-11-03 at about 16:43 GMT, cold, run by the user from the menu and filmed. Readings were 2 minutes after the first `tram` run, with no knob changed. Video is the user's `IMG_1302.mov`, and `M114` afterwards is in `test-logs/2026-11-03-tram-first/`.

| Corner | Wizard, at 25/205 (LCD, 2 decimals) | Tramline, at 30/200 |
|---|---|---|
| Front-left | +0.01 | +0.005 |
| Front-right | -0.02 | -0.035 |
| Back-right | -0.03 | -0.025 |
| Back-left | +0.04 | +0.053 |
| Range | about 0.07 | 0.088 |
| Verdict | Lower Back Left | Lower back-left |

- **Probe points are 25 and 205.** The LCD's position bar read nozzle X66.5/246.5 and Y32/212 at the corners. Taking off the probe offset gives X25/205 and Y25/205, which is `C29`'s mesh limits. `M114` agreed at the last corner. So the source's 30/200 was wrong for this printer, and `DefaultInset` is now 25.
- **Order confirmed:** front-left, front-right, back-right, back-left, shown on the status line.
- **Rule:** same verdict and corner, and the wizard's relative values show it subtracts the average. These were at different points, so the differences of up to 0.015 mm could be from that or from noise. Rerun at `--inset 25` to tell.
- **Wizard's output:** the status line showed only corner names, not `X: Y: Z:`, and nothing reached the server's log. So the wizard's values are only on the LCD, to 2 decimals. It didn't home again, as it was already homed.

**Repeatability,** 2026-11-03 at 16:52 to 16:54 GMT, cold (bed 23°C, nozzle 22.5°C), `--inset 25 --repeat 5`, about 10 minutes after the wizard, no knob changed. Took 125 seconds. Logs are in `test-logs/2026-11-03-tram-repeat/`.

| Round | Front-left | Front-right | Back-right | Back-left | Range | Verdict |
|---|---|---|---|---|---|---|
| 1 | 0.060 | 0.030 | 0.022 | 0.080 | 0.058 | Fail, lower back-left |
| 2 | 0.050 | 0.033 | 0.027 | 0.077 | 0.050 | Fail, lower back-left |
| 3 | 0.065 | 0.040 | 0.027 | 0.082 | 0.055 | Fail, lower back-left |
| 4 | 0.065 | 0.040 | 0.025 | 0.085 | 0.060 | Fail, lower back-left |
| 5 | 0.065 | 0.043 | 0.025 | 0.087 | 0.062 | Fail, lower back-left |
| Spread | 0.015 | 0.013 | 0.005 | 0.010 | 0.012 | |

- **Noise near the limit is real.** The range moved by 0.012 mm with nothing touched, and round 2 failed only by landing exactly on the limit. A bed within about 0.01 mm of the limit could go either way between runs. No margin has been added, as that's the user's decision.
- **Possibly drift as well as noise.** Front-right and back-left mostly rose through the rounds. Five rounds are too few to separate drift from noise.
- **Resolution:** readings fall on steps of about 0.0025 mm, which is one Z step at 400 steps/mm.
- **Against the wizard at the same points:** same corner every round. Average relative values were FL +0.010, FR -0.014, BR -0.026, BL +0.031, against the wizard's +0.01, -0.02, -0.03, +0.04. The wizard's range of about 0.07 is above all five rounds' 0.050 to 0.062. It's only one wizard run, 10 minutes earlier, so whether that's noise, drift or a real difference such as stowing is unknown.
- **Timing:** `G28` 14.4 s from low down, against 19.5 s from a fresh power-on. `G30` 4.9 to 5.5 s. Longest silence in the whole run 2.57 s.

**The extra move after each probe,** noticed by the user and filmed during round 3 of that run (`probe.mov`). After each reading the head moves on to the probe point's coordinates using the nozzle, so about 41.5 mm left and 7 mm forward, then goes to the next corner. The LCD shows it: X66.50 Y32.00 to probe, X44.89 passing through toward X25 Y25, then X246.50 Y32.00 for the next. It's `do_blocking_move_to(old_pos)` in `G30.cpp`, where `old_pos` is set to the requested X/Y, and it matches `M851`'s offset. The wizard doesn't do this. It happens after the reading, at Z5.9, so it's harmless and costs about a second per corner. `G30` has no option to skip it. The LCD's `Bed X: Y: Z:` line showed 0.065, 0.040 and 0.027, matching the readings Tramline logged for that round.

**Knob direction,** 2026-11-03 at 16:59 GMT, cold, `--inset 25`, one run after the user turned the back-left knob "a bit clockwise". Logs are in `test-logs/2026-11-03-tram-knob/`.

| Corner | Before, average relative of 5 rounds | After |
|---|---|---|
| Front-left | +0.010 | -0.008 |
| Front-right | -0.014 | -0.055 |
| Back-right | -0.026 | -0.036 |
| Back-left | +0.031 | +0.100 |
| Range | 0.050 to 0.062 | 0.155 |

**Clockwise from above, turning it to the right, raised the back-left reading,** by about 0.069 mm relative to the average, well outside the noise, so counter-clockwise from above lowers it. Only back-left has been tested. The user then turned it back by eye, so the bed isn't exactly where it was, and needs probing again. Absolute Z changes after homing again, so compare relative values. Moving one corner by d only moves its relative value by 0.75d. If the screw moves 0.7 mm per turn, which is unconfirmed, that turn was about 0.09 mm, or 47 degrees.

**Turn-size hints:** offered and declined by the user, who goes by the numbers. The LCD's menu items can also probe one corner at a time, which the user finds handy when adjusting. Only the differences between corners matter to the rule, not closeness to 0.

**Tramming cold has only been used to test the tool.** The real check is at print temperature, per Print flow, and that's the first heating above 60°C on the real printer.

**First hot `tram`,** 2026-11-03 at 17:13 to 17:14 GMT, `--inset 25`, after the user turned the back-left knob back by eye. Logs are in `test-logs/2026-11-03-tram-hot/`.

- **First heating above 60°C on the real printer:** `preheat 80 150` reached nozzle 150°C and bed 79.5°C in 257 seconds. During the 5-minute soak the nozzle held 149.91 to 150.09°C and the bed 79.99 to 80.19°C.
- **Readings:** front-left +0.077, front-right +0.045, back-right +0.043, back-left +0.102. Range 0.059 mm, fail, lower back-left. From average: +0.010, -0.022, -0.024, +0.035. One run, so the range could easily be 0.01 mm different.
- **Cooling and power off:** `cool` to a nozzle of 49.5°C took 238 seconds from 150°C, with the bed still at 62°C. `power off` worked, and USB dropped about 3 seconds later. Pi watch, 5 minutes: 57 checks, none bad. The kernel log for the whole session, in `test-logs/2026-11-03-tram-first/kernel.log`, had no errors, only the usual `urb stopped: -32` twice before the disconnect.
- **Run:** took 41 seconds, with the same step timings as cold.
- **After the user turned the back-left knob slightly left, about 2 minutes later:** front-left +0.072, front-right +0.053, back-right +0.048, back-left +0.080. Range 0.032 mm, **pass**. From average: +0.009, -0.010, -0.015, +0.017. Back-left came down by about 0.018 mm from the average, so the adjustment moved it the way the knob test predicted. This is the first real pass, one run only.
- **Five rounds hot, `--repeat 5`, straight after, no knob changed:** all passed, with ranges of 0.040, 0.035, 0.032, 0.034 and 0.030 mm. Spread by corner: front-left 0.000, front-right 0.005, back-right 0.010, back-left 0.002 mm. Took 124 seconds. The right-hand corners crept up over the first rounds and then settled, so it may be drift rather than noise, and it's tighter than the cold run's 0.005 to 0.015 mm. That's one set of five rounds each, cold and hot, so it doesn't show that hot is always quieter.
- **LCD wizard, hot, straight after, run by the user:** "Corners leveled, Tolerance achieved!" Front-left +0.01, front-right -0.01, back-right -0.02, back-left +0.02, range about 0.04 mm. Tramline's five-round averages were +0.012, -0.012, -0.017, +0.017, range 0.034 mm. Every corner agrees within the LCD's 2-decimal rounding, and both pass. Photo was sent by the user in chat, and isn't in the logs.

Still to confirm on the real printer:

- **Noise near the limit.** 0.05 mm is tight, so readings close to it may flip between passes. Measure the probe's repeatability with `--repeat`, and report that instead of inventing a margin.
- **Timing and replies.** Whether `busy:` really arrives during `G28` and `G30`, real durations, and whether `M118` echoes as expected.
- **`G30` versus the wizard.** `G30` stows the probe at each point, and the wizard may not. If the numbers differ, that's the first thing to try, with `E0`.
- **Knob direction.** M4 screws move about 0.7 mm per turn, so 0.05 mm is about 26 degrees. Confirm direction on the first run.

Adjustment loop:

1. Probe and report relative heights, plus the corner to change.
2. Park the head.
3. The user adjusts and tells Claude it's done.
4. Probe again, and only continue once it passes.

The mesh from `G29 P1` still corrects remaining tilt on every print. With a limit this tight, the check may stop for adjustment fairly often, so log every reading. Real data will show whether that's a problem, and it's the user's decision if it is.
