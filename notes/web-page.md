# Read-only web page

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

Built 2026-11-04, at the user's request. `tramline web` opens `http://192.168.1.121:8726/`: status, temperatures and progress, with the current job's file drawn in 3D, what's printed in orange, what's to come as a ghost, and a marker for the head. Drag to turn, right-drag to move, scroll to zoom, double-click to reset.

**On a phone:** open the same address, as `tramline web` only opens a browser on the machine running it. One finger turns, two fingers pinch to zoom and move together, and a double-tap resets. The panel gets its own part of the screen, at the top, or at the side with the phone turned, with a Details button for the rest, and the view frames the model to fit a tall screen.

**How it shares the port:** the first byte of a connection says which server gets it, in `splitListener` in `internal/server/web.go`. `0x16` starts a TLS handshake, and goes to the API with mutual TLS as before. Anything else goes to a separate `http.Server` with only the page's routes, so the API can't be reached from it, whatever the request. HTTPS in a browser fails, as a browser has no client certificate.

**What it exposes, with no login, to anything that can reach the port:** state, temperatures, the job's name, notes and errors, and the moves of the current job's file. It's plain HTTP, so anyone on the LAN can read them on the way. `WebStatus` is its own list of fields, so nothing added to the API becomes public by accident. Other files can't be listed or fetched. The port is still LAN only, by the `listen` setting.

**It can't affect a print:** it reads only what the server already holds in memory, and the uploaded file. It sends nothing to the printer or the Hue Bridge, and takes neither the operation lock nor the link. So plug state isn't shown, as reading it is a call to the bridge. Parsing the file takes about 90 ms for 92,794 lines on this machine, more on the Pi, is done once and kept in memory.

**The head's position is worked out, not measured.** `AUTOREPORT_POS` isn't in this firmware, and `M114` would wait in the queue, or can't be sent at all during a stream, which holds the link. So:

- The server reads the file's moves with `gcode.ParsePath`, using the same `Reader` and byte count as a job's position, and gives them to the page in a compact binary form.
- Times along the file come from its feed rates, scaled to the slicer's `;TIME:`.
- The page looks up the move at the job's position, steps back by `LAG_MOVES`, and glides the head there at the print's speed. Those are estimates of what's queued: 16 moves in a stream and 32 from the card, from the firmware's buffer sizes, not measured.
- **A stream** updates about once a second, as each line is answered, so it's close to live.
- **A print from the card** only has a position about every 30 seconds, and later if `M27` waits. Between them the page carries on at the file's speeds for up to 50 seconds, then waits. If it's more than 30 seconds out it jumps. So it's an estimate that gets corrected, and files under about 4 MB go to the card by default.

**Not drawn:** a print started from the LCD, whose file isn't on the Pi by that name, shows status only. The model is the file on the Pi, so a file replaced after its print shows the new one until the next job. The start G-code's heater waits and `G29` aren't in the times, as a job's position is what moves the head on.

**No libraries:** raw WebGL 2, embedded in the binary with `go:embed`, about 34 KB, in `internal/webui/`: the page in `page/`, and the 3D view in `shared/view.js`, which the GUI uses too, see Desktop GUI. It's a single page polling `/api/status` once a second, as polling recovers by itself from a server restart. A content security policy allows only its own files. `go test` doesn't run the JavaScript, so a change to it needs a look in a browser.

### Trying it on the simulator

This needs no Pi and no printer, and can run while the real server is printing, as it's all on this machine. After `./build.sh`:

```sh
./sim-web.sh ../projects/claude-monitor-case/stand-retro-print_PETG.gcode
```

It starts a simulated printer on `127.0.0.1:18726`, uploads the file, starts a print and opens the page in your browser. Ctrl-C ends it.

| Option | What it does |
|---|---|
| `--minutes <n>` | How long the print takes, default 10 |
| `--sd` | Prints from the simulated card, to see the position arrive about every 30 seconds, instead of streaming |
| `--no-open` | Shows the address without opening a browser |
| `--lan` | Listens on this machine's network address instead of `127.0.0.1`, and shows the address to open on a phone. A firewall on this machine may need to allow port 18726 |
| no file | Just the idle page |

The script keeps everything in a temporary folder that it removes at the end, including a throwaway client key, with `XDG_CONFIG_HOME` pointing there. So your real login isn't used, and the plain `tramline` command still talks to the Pi. To use the client on the simulator, for instance to try `cancel`, copy the line it prints, which starts with that `XDG_CONFIG_HOME`.

**What it can't tell you:**

- How close the drawn head is to a real one. The simulator has no real motion, so this only shows that the page follows a position.
- Temperatures from `--sd`: the simulator doesn't run a card file's lines, so the heaters stay where the tram check left them. A stream does follow the file's.
- Whether "Time left" is right. It comes from the slicer's times, so in a print sped up to 10 minutes it reads hours.
- The head jumps rather than glides in a sped-up print, as it falls over 30 seconds behind the file's own pace. The closer `--minutes` is to the slicer's time, the more it looks like a real print.

Plain `serve --sim` still has a bed that fails its tram check, which is how to see the page's `needs you` state. `--sim-level` gives it a bed that passes, `--sim-line-delay` sets each numbered line's time, which paces a stream, and `--sim-print-time` sets how long a card print takes.
