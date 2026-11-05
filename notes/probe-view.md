# Probe view: tram check and mesh, live

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Probe view

**Asked for by the user on 2026-11-05: a live view of the head during the tram check and during the mesh probing (`G29 P1`). Not started.** For the GUI, and the read-only page too if the user agrees, see Desktop GUI and Web page. The GUI polls, it doesn't push, see How requests reach the Pi. A one-second poll suits probing, which takes 2 to 5 seconds a point.

### What's known

- **Tram check:** Tramline drives it, so the server knows every step. `G28`, then `G30` at front-left, front-right, back-right, back-left, at X/Y 25 and 205 (see Tram check, "Probe points are 25 and 205"), then park at Z30 over X115 Y115. `TramJob` already has `Step` as text and `Rounds[].Readings` with each corner's X, Y and Z as it arrives. Real times: `G28` 14 to 20 s, each `G30` about 5 s. **The extra move after each probe:** the head moves on by the probe's offset, about 41.5 mm left and 7 mm forward, see Tram check. The view should show that, or at least not contradict it.
- **Mesh, from a real stream, `test-logs/2026-11-04-stream/t8-long/server.log`:** `Mesh invalidated. Probing mesh.`, then `Probing mesh point N/25.` for N = 1 to 25, 2.3 to 3 s apart, then a position report, `X:66.50 Y:212.00`. **No height is reported per point.** The 26 matching lines in each stream log show it happens on every print. `C29` gives the grid: 5×5 between 25 and 205 (see Tram check, the `C29` row).
- **The order of the mesh points isn't reported.** UBL probes the nearest unprobed point to the head, not row by row, so point N's position has to come from copying that rule, starting from the position before `G29`, here `X:156.50 Y:122.00` nozzle. Probe or nozzle coordinates, and the offset, need care. Check the result against the LCD or a video before trusting it.
- **Unknown:** whether these messages reach the Pi during a print from the card, and which read-only command prints the finished mesh's heights on this build: `G29 T`, `M420 V` or `M420 V1 T1`. `G29 W` printed nothing, see Tram check.
- **Not available:** `AUTOREPORT_POS`, and `M114` can't be sent during a stream. So, like the print view, positions are worked out, not measured.

### Plan

1. **Tram, server:** add to `TramJob` a structured target, for instance `phase` (homing, moving, probing, parking) and `target: {x, y}`, set as each command goes. Keep `Step` as it is for the CLI. Make the simulator's `G28` and `G30` take realistic times so the view can be seen.
2. **Tram, view:** in `shared/view.js`, markers at the four probe points. The head glides to the target at a plausible speed, and the probe drops. Each reading becomes a pin, scaled up so 0.05 mm shows, coloured by distance from the average. The verdict and knob advice as in the GUI now. A print's own tram check (`PrintJob.Check`, the `checking` state) should use the same view.
3. **Mesh, server:** in a stream, parse `Probing mesh point N/M.` into the print job, for instance `mesh: {point, of, started_at}`, with a test from the real log's lines. Expose it in `WebStatus` only if the user agrees to the page showing it.
4. **Mesh, view:** a 5×5 grid of points over the bed, filling in as N rises, and the head at point N by the copied UBL order, marked as an estimate.
5. **Mesh heights, needs the printer:** with the user's OK, printer on, nothing moving, try `G29 T` and `M420 V` and record the output in Firmware findings. If one works, `tram info` and a "Show mesh" button can draw it as a coloured surface. Adding it to every print's stream after `G29 P1` changes the print, so that's the user's decision.
6. **SD prints:** check in the log of a real SD print whether the messages arrive. If not, the mesh view is for streams only.

**Rules:** build only with `./build.sh`. Try everything on `./sim-gui.sh` first, checking in headless Chrome as in Desktop GUI. Ask before every test involving the printer, saying what it does and how long.
