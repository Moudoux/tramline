# Firmware findings

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

Read from [the firmware source](https://github.com/mriscoc/Ender3V2S1) at commit `05903a8` (19 Sep 2026). **The printer's build is from Dec 2023, so all of this needs testing on the real printer.**

### Tramming

- `C35` launches the [tramming wizard](https://github.com/mriscoc/Ender3V2S1/wiki/Firmware-features), and `C35 T<n>` probes a single point.
- The readable code, `tram()` in `bed_tramming.cpp`, reports to the LCD only. The wizard itself, `trammingWizard()`, is in the closed-source `libproui` library.
- **The wizard's tolerance is 0.05 mm.** The float `0.05` is in `trammingWizard()`'s compiled code and in `bed_tramming_probe_tolerance`. This was read from `lib/proui/stm32f1/libproui_ubl_dwin.a` at tags `20231202` and `20240122`, either side of this printer's build, and at current `main`.
- The comparison logic comes from public Marlin 2.1.2.1, the same wizard before it went closed-source. It subtracts the average from the four readings, then passes if `max - min < 0.05`. Otherwise it names the corner furthest from the average, with "Lower" if above average and "Raise" if below.
- The library's probing order is front-left, front-right, back-right, back-left.
- `G35` is disabled because `ASSISTED_TRAMMING` is commented out.
- **`G30 X<x> Y<y>` is the route to use.** It sends `Bed X:.. Y:.. Z:..` over serial.
- `G30` homes if needed, and turns bed levelling off. The start G-code turns levelling back on with `G29 A`.
- **Tramming insets, since found to be wrong for this printer, see Tram check:** `tram()` prefers `BED_TRAMMING_INSET_LFRB`, which `Configuration.h` at tag `20231202` sets to 30 mm per side, so 30 and 200. It only falls back to the mesh limits (default `MESH_INSET` 25 mm) if that isn't defined. That file is the repo default and may differ from this printer's build, and the wizard is closed-source. 
- `tram()` sets the LCD status to `X: Y: Z:` for each point, which may show the wizard's points and values. `HOST_STATUS_NOTIFICATIONS` is off in that config, so they probably don't reach serial.
- `G30` ends in a normal `ok` even when it fails, with `Error:Probing Failed` or `Z Probe Past Bed` and no `Bed` line. It uses `G28O`, which homes only if needed.
- `C29` sets mesh insets, per the wiki. What it does with no parameters is unknown, so Tramline doesn't send it.
- Default speeds at that tag: homing 50 mm/s X/Y and 10 mm/s Z, probe travel 200 mm/s, Z probing 16 mm/s, and `busy:` every 2 seconds. Steppers switch off after 120 seconds idle.
- A [firmware issue](https://github.com/mriscoc/Ender3V2S1/issues/1186) reports that the wizard fails on a severely tilted bed. Manual tramming comes first in that case.

### Mesh

Read 2026-11-06 from the firmware's source at tags `20231202` and `20240122`, either side of this printer's build of 17 Dec 2023. Between the two tags the files below only changed in debug output, so this should hold for the printer, but **none of it has been seen on the real printer yet**.

- **`M420 V1 T1` only prints,** and is what Tramline sends (`marlin.MeshCommand`). In `M420.cpp`, with no `S`, `L` or `C`: `V` prints the map through UBL's `display_map`, with `T1` as numbers separated by tabs, then "Mesh is valid" or "Mesh is invalid" and "Storage slot: N". Then it sets levelling to the state it already has, which `set_bed_leveling_enabled` skips when nothing changes, and prints "Bed Leveling ON" or "OFF" and the fade height. An invalid mesh adds "echo:Invalid mesh." and nothing else. `S` would turn levelling on or off, `L` load a mesh from storage, and `C` shift the mesh, so Tramline never sends those, and `Z` sets the fade height.
- **`G29 T1` prints the same map,** and with no `P`, `J`, `A`, `D`, `F`, `I`, `Q`, `L` or `S` it does nothing else: `may_move` is only true for `P1`, `P2`, `P4` or `J`. But it's the command that probes, so a damaged `T` could do far more than a damaged `M420`. It isn't used.
- **Why `G29 W` printed nothing:** its report, `g29_what_command`, is only built with `UBL_DEVEL_DEBUGGING`. So `tram info`'s `G29 W` can go.
- **The map's layout:** "Bed Topography Report for CSV:", a blank line, then a row for each Y from the back (Y205) to the front (Y25), each with X from left (25) to right (205), to 3 decimals, `NAN` where a point has no height.
- **What a read after power-on shows:** at boot the settings load restores the storage slot (`M503` said "Active Mesh Slot 0") and, as the slot is 0 or more, loads that slot's mesh from EEPROM (`settings.cpp`, `load_mesh(bedlevel.storage_slot)`). So it's the mesh last **saved**, by the LCD's Save Mesh, `G29 S` or `M500` if the build saves the mesh with it, and not the last print's: the start G-code's `G29 P1` builds a mesh in memory and never saves it, and Tramline cuts the plug after every print. When that slot was last saved isn't known. Levelling is off at boot (`M420 S0` in `M503`).
- **What a read just after a print shows:** that print's own mesh, as long as the printer hasn't been turned off. Tramline waits for the nozzle to fall below 50°C before cutting the plug, a few minutes after a print, and `tramline mesh` is allowed then, so it can read it without changing the print.
- **Seen on the real printer, 2026-11-06, 10:33 GMT, after power-on,** logs in `test-logs/2026-11-06-mesh/t1-power-on/`: `M420 V1 T1` answered exactly as worked out above, a blank line, "Bed Topography Report for CSV:", a blank line, five rows of five tab-separated heights, "Mesh is valid", "Storage slot: 0", "echo:Bed Leveling OFF", "echo:Fade Height 10.00", `ok`. The boot text, logged as "printer answered something earlier", includes "Unified Bed Leveling System v1.01 active" and **"Mesh 0 loaded from storage."**, which confirms the boot load, though levelling then reads OFF, as `M503`'s `M420 S0` says. `M503` was unchanged from 2026-11-03.
- **The stored mesh,** back row first, X 25 to 205 left to right:

  ```
  -0.065  +0.002  -0.024  -0.027  -0.067   Y205
  +0.032  +0.069  +0.032  +0.029  -0.050
  +0.012  +0.057  -0.002  -0.004  -0.077
  -0.014  +0.029  +0.000  +0.012  -0.042
  -0.050  -0.002  -0.022  -0.012  -0.075   Y25
  ```

  Range 0.146 mm. Its four corners are within 0.025 mm of each other (front-left -0.050, front-right -0.075, back-right -0.067, back-left -0.065), so it was saved with the bed close to tram, and the highest part is inside, left of the middle, at X70 Y160, +0.069. When it was saved still isn't known.
- **A print's own mesh, seen 2026-11-06 at 10:48:00 GMT,** read with `tramline mesh` 4 s after the test tile finished from the card, the nozzle at 227°C, logs in `test-logs/2026-11-06-mesh/t2-sd-print/`. Valid, slot 0, **levelling on**, range 0.090 mm:

  ```
  +0.027  +0.035  +0.002  +0.045  +0.020   Y205
  +0.075  +0.072  +0.017  +0.048  -0.007
  +0.033  +0.033  -0.002  +0.027  -0.015
  +0.062  +0.055  +0.012  +0.048  +0.017
  +0.050  +0.055  +0.022  +0.067  +0.045   Y25
  ```

  It differs from the stored mesh by up to about 0.12 mm (front-right corner -0.075 stored, +0.045 now), so the stored one is old, or from another bed state. UBL measures each mesh against the `G28` Z home in the middle, so the middle is near 0 in both.
- **The LCD during `G29 P1`,** seen on video 2026-11-06: a "Bed Leveling" grid fills in cell by cell with each height to 2 decimals, back row at the top, then the print screen. So the LCD has the heights point by point, and serial doesn't.
- **Heights per point:** during `G29 P1`, ProUI's `dwinMeshUpdate` puts "Probing point N/25 Z=…" on the LCD's status line, with the height. Serial only gets "Probing mesh point N/25.", with no height.
- **The LCD after `G29 P1`:** `dwinLevelingDone` opens the mesh viewer, or, in the extended ProUI build, a closed-source `levelingDone`. Whether either saves the mesh by itself isn't known, which matters to what a boot loads.

### Serial and host behaviour

- Default baud rate is 250000, changeable with `M575`.
- Buffer sizes in source: `BUFSIZE 16`, `TX_BUFFER_SIZE 64`, with `ADVANCED_OK` on. The live printer reported `ok P15 B15`.
- Emergency parser is on, so emergency commands get through even while the printer is busy.
- Per the wiki, cancel an SD print with `M524` rather than `M25`, and Meatpack is incompatible.
- The source enables `HOST_KEEPALIVE_FEATURE`, so long commands may send `busy:` messages. Timeouts must allow for this.
- The wiki describes `M75` and `M76` for host print timers. They aren't needed for SD prints unless testing shows otherwise.

### Custom codes

From the wiki:

| Code | Purpose |
|---|---|
| `C10` | Marks a file as a configuration file, skipping the print confirmation and print-done dialogs |
| `C35` | Launch tramming wizard |
| `C108` | Cancel screens waiting for a simple confirmation |
| `C115` | Show ProUI details |
| `C510` | Lock or unlock the screen |
| `C851` | Probe feed rate and multiple probing |

**Possible blocker:** because `C10` exists to skip a print confirmation dialog, a remotely started print may stop and wait at the LCD. Test this, and try `C108` if it does.
