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
- **A stream** moves its position on as each line is answered, and the page has it within 200 ms, see Status streams. It was once a second before 2026-11-21.
- **A print from the card** only has a position about every 30 seconds, and later if `M27` waits. Between them the page carries on at the file's speeds for up to 50 seconds, then waits. If it's more than 30 seconds out it jumps. So it's an estimate that gets corrected, and files under about 4 MB go to the card by default.

**Not drawn:** a print started from the LCD, whose file isn't on the Pi by that name, shows status only. The model is the file on the Pi, so a file replaced after its print shows the new one until the next job. The start G-code's heater waits and `G29` aren't in the times, as a job's position is what moves the head on.

**No libraries:** raw WebGL 2, embedded in the binary with `go:embed`, about 34 KB, in `internal/webui/`: the page in `page/`, and the 3D view in `shared/view.js`, which the GUI uses too, see Desktop GUI. It's a single page following `/api/events`, see Status streams below, which the browser reconnects by itself after a server restart. Against a server with no stream it polls `/api/status` once a second. A content security policy allows only its own files. `go test` doesn't run the JavaScript, so a change to it needs a look in a browser.

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

## Status streams

**Built 2026-11-21, `go test` and headless Chrome on the simulator only, not installed.** The user found both the read-only page and the GUI slow to show a print's movement and the probing, as they polled: the page once a second, and the GUI's tram and mesh views, which come from `GET /v1/status`, only every 2 seconds, as each of those calls asks the Hue Bridge for the plug.

- **Server, `internal/server/events.go`:** `GET /api/events` on the web page (`WebStatus`) and `GET /v1/events` on the API (`StatusResponse` without the plug) are server-sent event streams of `status` events. Each stream reads the server's memory every 200 ms and sends when something has changed, and at least once a second, so clocks and the card's coasting estimate keep going. Fields that change on every read don't count as news: `moved_ago`, `run_seconds`, `step_seconds`, `point_seconds` and `temps_at`. The temperatures themselves do. Nothing in a stream asks the printer or the Hue Bridge anything. The web server's one-minute read and write timeouts would end a stream, so it clears the read deadline and gives each event 10 seconds to send. A stream ends when the browser goes or the server stops.
- **Browser, `followStatus` in `shared/status.js`:** used by both pages. `EventSource` reconnects by itself after a dropped connection. Quiet for 5 seconds shows "can't reach", quiet for 15 reopens it. A stream refused before it sent anything is followed by one poll, which tells an older server, which then gets polled as before, from one out of reach, whose stream is retried every 2 seconds. Streams close while the tab is hidden.
- **Measured on the simulator, 2026-11-21:** during a stream from the Pi, the head's position arrived 4.6 times a second on both pages, against once a second before. The GUI's full stream averaged 1.3 events a second over a tram check and a mesh, and each tram step and mesh point arrived as it happened, against every 2 seconds before. Idle, each stream sends once a second or so. Both fallbacks were checked by pointing `followStatus` at a missing stream.
- **Tests:** `TestWebEventsPushAChangeStraightAway`, `TestEventsWithNoNewsComeAboutOnceASecond`, `TestAPIEventsFollowATramCheckWithoutThePlug` and `TestWebEventsAreOnThePageOnly`, and the GUI's route tests include `/pi/events` and `GET /v1/events`.
- **Not proven:** the real Pi, over the LAN and Tailscale, and whether 200 ms feels live enough. With a stream, the card's position still only changes when `M27` answers, about every 30 seconds, so a print from the card looks the same as before. The read-only page still has no tram or mesh view, as `WebStatus` doesn't carry them, which is the user's decision, see Probe view.
