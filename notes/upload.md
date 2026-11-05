# Upload and SD copy

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

**Built 2026-11-03 in `internal/gcode`, `internal/marlin/sd.go` and `internal/server/upload.go`. A small copy has passed on the real printer, see below. The firmware behaviour below comes from its source at tag `20231202`, whose build is close to this printer's from 17 Dec 2023. Only what's listed under the real-printer test is confirmed.**

**Real-printer test,** 2026-11-03 at 18:24 to 18:31 GMT, with `dev-20261103175429`. Logs are in `test-logs/2026-11-03-sd-upload/`.

| Item | Result |
|---|---|
| File | `sd-test.gcode`, generated `M118` lines only, 2,000 lines, 76,000 bytes on the card as `SDTEST28.GCO` |
| Saving on the Pi | 30 ms |
| Copy | 10.45 s with no resends or timeouts, so **192 lines a second, or 5.2 ms a line**, and 7.3 KB/s |
| `M20` size | Exactly the 76,000 bytes predicted, so the CR LF rule holds |
| `M28`, `M29`, `M20`, `M30` | All replied as the code expects. The card's listing afterwards matched the one before, and files in folders parse |
| Temperatures during the copy | Kept updating about every 2 seconds |
| `power off`, Pi watch for 5 minutes | USB dropped about 2 seconds later. 57 checks, none bad. Kernel log showed only the usual `urb stopped: -32` twice before the disconnect |

- **Not checked:** the copy's contents can't be read back over serial, so only its size is verified. The LCD wasn't reported on.
- **Not exercised:** resends and every failure path, as nothing went wrong. Also, this was one copy of one small file, of slightly longer lines than sliced G-code, so large-file speed is an extrapolation.
- **Where the time goes:** a test line takes about 1.9 ms to send and its `ok` about 0.7 ms, so about 2.6 ms is USB and firmware delay. Sending several lines at once removes most of the waiting, but the board's receive buffer size is unknown.

**Two modes, decided by the user.** Every file is saved on the Pi first, so the client machine can then go away. After that:

| Mode | What happens | Good for | Risk |
|---|---|---|---|
| `auto`, the default | SD if the copy would take no longer than the limit, otherwise stream | | |
| `sd` | Copied to the card, then printed from it | Survives a Pi hang, restart or lost link | A wait before printing |
| `stream`, **printing not built yet** | Stays on the Pi, and is sent line by line during the print | Starts at once | The print stops if the Pi or link fails, possibly with a hot nozzle sitting on the part |

`tramline mode` changes the default, which is kept on the server. `upload --sd` and `upload --pi-only` override it once. OctoPrint streams by default, and its optional SD upload uses the same `M28` method and is slow too, going by memory rather than testing.

**Flow:**

1. **Save.** `PUT /v1/files/<name>` with a SHA-256 header stores the file in `/var/lib/tramline/uploads/`, with a JSON record beside it. It works with the printer off, and doesn't touch the printer.
2. **Check.** Comments and blank lines are stripped, and a file is refused if a line has `*`, `\`, control or non-ASCII bytes, `M28`, `M29` or `M110`, or is over 80 characters. Largest upload 256 MB, and 1 GB of disk must stay free. All 18 sliced files scanned would pass, with a longest line of 40 characters.
3. **Copy.** Runs in the background. `N0 M110 N0`, `N1 M28 <name>`, the numbered lines, then a numbered `M29`. Since `dev-20261103184515`, up to 120 bytes may be unanswered at once, about 3 lines, see Window below. Stage 8's real test sent one line at a time.
4. **Verify.** `M20`'s size must exactly match the sum of each stripped line plus CR LF. That catches lost lines but not a same-length change on the card, which the checksums make unlikely.

**What the firmware does, and what it means here:**

| Firmware | Consequence |
|---|---|
| `M28` aborts a running SD print | `printerBlocker`'s `M27` runs straight before it, and anything unknown refuses |
| While saving, an unnumbered command gets an error, `Resend` and `ok`, and doesn't run. Only `M29` is accepted | `cool`, `M27` and `M118` can't run mid-copy. The link tries `M29` first if a save may be open |
| The save stays open if the link drops while the board has power | The handshake sends `M29` on the "No Checksum" error, otherwise it would never reconnect |
| The emergency parser is off during a save | `M112` won't work mid-copy. **`estop` must cut the plug or send `M29` first** |
| A repeated line number gets no reply at all | After a timeout, send the *next* line. The firmware takes it, or asks for the missing one |
| `M28` gives a plain `ok` with no card mounted | `Writing to file:` must be seen |
| `;` starts a comment before the checksum is read | `;TIME:` can't be sent as written, so comments go, and the LCD may lack time estimates. `\;TIME:` looks possible from the source, and hasn't been tried |
| `M29` anywhere in a line closes the file, and the first `*` ends the saved text | Such files are refused |
| `M28` empties an existing file, and there's no rename | Partial files stay under the real name. A name already on the card that didn't come from here is refused |

**Operation lock:** the copy holds it from start to finish, as nothing else could run then. `tram`, `preheat`, `tram info`, `power` and `files rm` are refused straight away. `status` still works, and `files` leaves out the card. A copy is refused during a tram run or a print.

**Idle timer:** a copy doesn't extend it. `cool` and an expired timer stop the copy with `M29` and then take the lock, so heaters always win. If `M29` fails, the existing plug cut logic applies. During a copy the idle check skips `M27` and relies on temperature reports continuing, which they did on the real printer.

**Failures:** a dead board fails after 3 timeouts in a row at 5 seconds each, or 10 resends with no progress. Any failure deletes the partial file with `M30` if the link is up. If it isn't, the record says a partial file may remain, as the LCD could still start it. There's no automatic retry or power change. A record left as running after a restart reads as failed.

**Window,** added 2026-11-03 at the user's request, tested on the real printer, see below. Each `ok` frees its line's share of the window. The firmware's `stm32_serialbuffer.py` gives this board a 128-byte receive buffer, assuming the official build used the default, so 120 bytes can't overflow it even if a card write stalls the board. `ok N<line>` answers everything up to that line, and a plain `ok` answers the oldest. Ending a save early sends `M29` and then waits for an `M118` marker's echo, because an `ok` could be for an earlier line, and the firmware refuses the marker during a save. After `Resend`, sending stops until replies have been quiet for 200 ms, then restarts from the line asked for. After silence, it sends on, as with one line at a time. `copy --window <bytes>` overrides it for measuring: 1 is one line at a time, and the most is 2048. Past 128 bytes, an overflow is possible, and would be caught by the checksums and resent.

**Retrying:** run `tramline upload <file>` again, or `tramline copy <name>` from any client. The file comes from the Pi, and a verified copy whose size still matches is skipped. That relies on the record and `M20`'s size, so a swapped card with the same name and size would fool it.

**Names:** 8.3 for now, such as `PIDECK3F.GCO`: up to 6 letters and digits from the name, then 2 hex digits from its hash. Long names are enabled in the firmware but untested.

**Binary transfer:** the firmware reports `BINARY_FILE_TRANSFER`, but the wiki describes it for firmware upgrades. Treat it as a later speedup.

An SD card must stay in the printer.

**Window test on the real printer,** 2026-11-03 at 18:47 to 18:55 GMT, with `dev-20261103184515`. Logs are in `test-logs/2026-11-03-sd-window/`. Every copy's size matched on `M20`, and the card's listing afterwards matched the one before.

| File | Window | Time | Speed | Lines a second | Resends |
|---|---|---|---|---|---|
| 2,000 `M118` lines | 1 byte, one line at a time | 10.47 s | 7.3 KB/s | 191 | 0 |
| Same | 120 bytes | 5.42 s | 14.0 KB/s | 369 | 0 |
| Same | 240 bytes | 6.30 s | 12.1 KB/s | 317 | 10 |
| Same | 480 bytes | 5.86 s | 13.0 KB/s | 341 | 8 |
| `test-esp-jig_PETG.gcode`, 15,264 lines | 120 bytes | 31.28 s | 14.0 KB/s | 488 | 1 |

- **120 bytes stays the default.** It was the fastest, and going past the buffer only caused resends. It's one run of each, so small differences mean little, but a window much above 120 shows no gain.
- **Speed limit:** the sliced file used about 19.5 of the link's 25 KB/s, so little more can be gained on this link.
- **Resends past the buffer:** at 240 and 480 they came about every 216 lines, or about 8 KB of file. That fits the board stalling every 8 KB on the card and overflowing its receive buffer, which supports the 128-byte figure. The stall's cause is a guess.
- **The one resend at 120 bytes,** at line 1343 of the sliced file, isn't explained. An overflow shouldn't be possible at that window, and it happened once where the file crossed about 53 of those 8 KB points. It may be a damaged line on the wire. The firmware's error text wasn't logged then, and now is. The copy recovered and its size matched.
- **`ok N2 P15 B15`** confirmed as the reply format on the real board.
- **Resends worked on real hardware,** 19 times, always with the right size afterwards. Size is all that can be checked.

**Copy times,** estimated from this test. The low figure scales by lines at 488 a second, the high one by size at 14 KB/s, using one file's measurement:

| File | Lines | On the card | Estimated copy |
|---|---|---|---|
| `test-esp-jig_PETG.gcode` | 15,264 | 0.44 MB | 31 s, measured |
| `pi-deck-slide_PETG.gcode` | 130,461 | 4.2 MB | about 4.5 to 5 min |
| `stand-print_PETG.gcode` | 389,414 | 12.7 MB | about 13 to 15 min |
| `stargate_gate_PETG.gcode` | 885,702 | 28.6 MB | about 30 to 34 min |

**Choosing a mode:** `auto`, the default since the user chose it, copies a file to SD unless the estimate at 14 KB/s is over the limit, 5 minutes unless changed with `tramline mode auto --limit <minutes>`. So files up to about 4.2 MB go to the card. Larger ones stay on the Pi for streaming, which **isn't built yet**, so until then they can't be printed that way. `tramline copy <name>` or `upload --sd` copies them anyway. The choice is made by `upload` from the size alone, and nothing checks the estimate against the real time.

**Before streaming is used for long prints:** a hung Pi would stop a print with the nozzle possibly still hot on the part, and that Pi has hung once. Streaming needs line numbers on every line, and a safeguard to look for in the firmware, such as host-silence handling, which hasn't been checked.

**Simulator tests:** corrupted, lost and duplicate lines, lost replies, `M29` lost or corrupted, write errors, no card, a silent board, link drops, reconnecting into an open save, power loss mid-copy, cancelling, `cool` and the timer stopping a copy, every refusal, and the CLI end to end with a real sliced file. The simulator was written from the firmware source, so it can share any misreading of it.

**LCD during a copy:** it showed the file name, reported by the user, as the source's `ui.set_status` suggests.

**Still to confirm on the real printer:** the speed of a large file, and `M29` at reconnect.
