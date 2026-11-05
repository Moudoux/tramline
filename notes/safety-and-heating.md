# Safety rules and heating

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Safety rules

- Ask for bed-clear confirmation before every print, since there's no camera and a leftover part could be hit. The server refuses `print` without it.
- Turn heaters off after 30 idle minutes. See Heating below. Simulator only so far.
- Park the head before knob adjustments, and remind the user that the bed is hot.
- `power off` is refused while the heaters are on, the nozzle is at 50°C or above, or a connected printer's temperatures aren't known yet. Done.
- Never cut power while the nozzle is above about 50°C, so the hotend fan can keep cooling it, except for an emergency stop.
- Never cut power during a print, except for an emergency stop.
- Never send heat commands when the plug is off, as a failed heat-up could trigger a thermal error.
- Enforce temperature limits on the server: bed 100°C and nozzle 260°C, decided by the user. Anything above them is rejected. A stock hotend's PTFE liner starts to degrade at around 250°C, so the limit is above what this hotend should run at for long.
- Keep these rules in the server, so they hold for every client and if a client disconnects. A lost client must not leave heaters on indefinitely, so the idle timeout applies.
- Rely on the firmware's `THERMAL_PROTECTION`, and don't try to replace it in software.
- Have the service recover cleanly after a restart, by reading the printer's state and not assuming anything.

## Heating

Built 2026-11-03 in `internal/server/heat.go` and `internal/marlin/printer.go`.

Tested on the real printer on 2026-11-03 at 15:14 to 15:22 GMT, with the user watching. Logs are in `test-logs/2026-11-03-heating/`.

| Test | Result |
|---|---|
| `preheat 40 60` | Accepted, with the targets read back. Nozzle 60.0°C and bed 39.8°C after about 59 seconds |
| `power off` while heating | Refused, "the heaters are on". Plug stayed on |
| `cool` | Targets 0 straight away |
| `power off` just after `cool` | Refused, "the nozzle is 60 C" |
| Nozzle cooling | 60°C to 49.2°C in about 48 seconds. Bed still rose to 40.4°C after `cool`, as it lags |
| `power off` at 49.2°C | Worked, USB dropped about 2.5 seconds later |
| Pi watch, about 5 minutes after | 57 checks of ping, login, `sync`, throttling and failed units, none bad. Kernel log showed no errors, only the usual `urb stopped: -32` twice before the disconnect |

`M27` was accepted without an error, so its idle reply matches what the code recognises. The reply while printing hasn't been seen. The temperatures used were low, so high-temperature behaviour is still untested.

**Idle timeout,** tested on the real printer on 2026-11-03 at 15:24 to 16:00 GMT, with `dev-20261103151749`. Logs are in `test-logs/2026-11-03-idle-timeout/`.

| Test | Result |
|---|---|
| `preheat 40 60` at 15:24:20 | Server gave 15:54:20 as the shutoff |
| Polling `status` every 2 seconds for 30 minutes | 849 samples, all with the same shutoff time, so polling didn't extend it. No failed samples or warnings |
| Shutoff | Server logged `heaters off, idle timeout` at 15:54:23.8, 3.7 seconds after the limit, with no client command |
| `power off` at 49.4°C | Worked, about 50 seconds after the shutoff |
| Holding temperature, 16:30 local to shutoff | Nozzle 59.91 to 60.18°C, bed 39.92 to 40.17°C |
| Pi watch, about 5 minutes after | 62 checks of ping, login, `sync`, throttling and failed units, none bad. Kernel log showed no errors, only the usual `urb stopped: -32` twice before the disconnect |

**Late `M115` reply, fixed 2026-11-03:** waiting 10 seconds didn't remove the 45 log lines, because an early `ok` ended the first `M115` wait just before the reply arrived. The handshake now ends only on an `ok` after the firmware line. On the real board that gave no `Cap:` lines and connected at 15:24:10.4, about 5.2 seconds after the startup text. That's only about 0.5 seconds faster than before. The early `ok` follows the board's own startup output, ending `echo: Serial 0 baud rate set to 250000`, with no `Unknown command`, so it isn't the reply to the garbage bytes. Which command it answers is unknown.

**Boot banner seen:** on this power-on the server caught the board's startup text, including `start` and `External Reset`, about 0.8 seconds after USB appeared. So the earlier unconfirmed note that `start` is sent before USB is up doesn't always hold. It's unknown why it said `External Reset` for a power-on, or whether opening the port played a part.

- **`preheat`** needs the plug to read on and the printer to be connected with known temperatures and no SD print running. Anything unknown refuses. It sends `M140`, `M104` and `M105`, and checks the reported targets. If that fails, it tries to turn both heaters off.
- **`cool`** always sends the off commands if connected, even during an SD print, as it's a safety command. With no connection, it succeeds only if the plug is confirmed off.
- **Idle timeout:** decided by the user, only a new `preheat` restarts it, so a polling script can't keep the heaters on. The check runs every 5 seconds and goes by the targets the printer reports, so heaters set from the LCD, or found on after a server restart, also get 30 minutes. A running SD print, checked with `M27` every 30 seconds, keeps restarting the timer.
- **Last resort,** decided by the user: if the timer has expired and the heaters can't be turned off, because the link is down or the board isn't answering, the plug is cut 1 minute later. The hotend fan stops too, which is accepted as the lesser risk. If the last `M27` showed an SD print running, the plug is left on and `status` shows a warning, because an SD print survives losing the link. A lost link with a print that started less than 30 seconds earlier could be missed.

**What the timeout can't cover:**

- If the Pi hangs, nothing turns the heaters off, and only the firmware's thermal protection remains.
- `systemctl stop` leaves the heaters on with no timer. After a crash or restart, the timer starts again at a full 30 minutes once the printer is connected.
- If the server restarts while the USB link is also down, it doesn't know the heaters were on.
- The plug cut has only run against a fake plug. Testing it for real means pulling the USB cable while heating.
