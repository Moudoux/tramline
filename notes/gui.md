# Desktop GUI

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Desktop GUI, `tramline gui`

**Built 2026-11-05, simulator only.** Committed 2026-11-05. Not run against the real Pi, and not known to be installed there. The user chose a local web GUI over a native Qt app or a webview wrapper such as Wails, as it keeps the build pure Go, so the macOS binary still cross-compiles from Linux, and it reuses the 3D view. **What the user asked for:** everything the client CLI does, including uploading files from the browser, in the read-only page's theme, sleek, modern and intuitive.

**Shape:** `tramline gui [--no-open] [--port <n>]` runs on the client machine, not the Pi. It starts an HTTP server on `127.0.0.1`, on a port the system picks, opens the browser at it, and runs until Ctrl-C. The browser only talks to that server, which talks to the Pi with the existing `internal/client` code and the client certificate. So the key never reaches the browser, and **nothing changes on the Pi:** its safety rules, such as the bed-clear confirmation and refusing power off while hot, still decide, and the GUI shows the Pi's errors as they come. Stopping the GUI stops nothing on the Pi.

### Code

- `gui_cmd.go`: the command, and `openBrowser`, now shared with `tramline web`. Both use `$BROWSER` if it's set.
- `internal/gui/gui.go`: authentication, the route list, the Pi page's status and model, and uploads. Tests in `gui_test.go`.
- `internal/webui/`: every browser file, embedded by both binaries' roles. `page/` is the read-only page the Pi serves, `gui/` the GUI, and `shared/` what both use, served under `/shared/`: `view.js` (the WebGL renderer and the `PrintView` class that follows a job), `status.js` (state words and formats), `theme.css` (the colours). The Pi serves only `page/` and `shared/`, which a test checks.
- `internal/server/web/viewer.js` became `shared/view.js` plus `page/page.js`, with no change in what the read-only page does. That was checked in headless Chrome on the simulator.
- Small additions: `client.Transport()`, `client.Login()`, `client.Explain()` (the "allow this client" message), `server.ValidFileName()`, and `Renderer.offsetX` in the shared view, which the GUI uses to centre the bed beside its panel.

### Authenticating the browser

The pattern Jupyter uses, each rule with a test:

- It listens on `127.0.0.1` only. `New` refuses anything else.
- A random 32-byte token in the address it opens, `http://127.0.0.1:<port>/?t=<token>`. The first request with it gets an `HttpOnly`, `SameSite=Strict` session cookie and a redirect to `/`, and **the token stops working,** as on Linux the address is briefly visible in `ps`. A second tab needs `tramline gui` to be run again.
- Every request needs the cookie, compared in constant time. The cookie's name includes the port, as cookies don't keep to one port.
- `Host` must be exactly `127.0.0.1:<port>`, which stops DNS rebinding.
- Anything but `GET` or `HEAD` needs `Origin` to be exactly `http://127.0.0.1:<port>`. A `Sec-Fetch-Site` other than `same-origin` or `none` is refused for every method.
- A content security policy like the page's, so no outside scripts.
- **The sign-in address is only printed to a terminal:** `--no-open` refuses otherwise, and a failed browser launch only shows it in a terminal. The global rule against printing secrets to non-interactive output covers this. The address is passed to the browser launcher as an argument.
- **What it doesn't add:** anything running as the user could already use `client.key`, so the local server sits behind the same line.

### How requests reach the Pi

- `/v1/...` goes to the Pi over mutual TLS **only for the method and path pairs in `APIRoutes`,** those the CLI uses. Not `PUT /v1/files/...`, as uploads go through the GUI's own endpoint, and not `PUT /v1/update`. Cookies, `Origin`, `Referer`, `User-Agent` and `Sec-*` headers aren't passed on, and the Pi's `Set-Cookie` isn't passed back. JSON bodies are limited to 1 MB.
- `/pi/status` and `/pi/model/{id}` come from the Pi's plain-HTTP page, which answers from memory.
- **Polling:** `/pi/status` once a second, every 5 seconds with the tab hidden. `GET /v1/status` every 5 seconds, every 2 while a tram check, copy or print preparation runs, and not at all with the tab hidden, **because each call asks the Hue Bridge for the plug's state.** `GET /v1/files` reads the card's listing from the printer, so it's only asked for when the Files tab opens, after an action, when a job starts or ends, and by the refresh button.
- **Upload:** `POST /gui/upload?name=...` with the file as the body. The GUI spools it to a private temporary folder, hashes it, skips it if the Pi already has that hash, as `tramline upload` does, and otherwise sends it with `PutFile`. `GET /gui/upload` gives the progress, which the page shows as reading for the first tenth of the bar and sending for the rest. One at a time. A name the Pi won't take is offered as a cleaned-up one, such as `fit test (v2).gcode` becoming `fit_test_v2.gcode`. Replacing a file by the same name asks first.
- `/gui/info` gives the page the version, the Pi's address and limits it uses: maximum upload, tram tolerance, copy speed and the shortest schedule lead.

### The page

- **Layout:** the read-only page's look, with the 3D view across the window and a panel on the left. At the top a status card: file, progress, layer, time left, temperatures and plug, and the buttons for what's running now. Below it tabs: **Files** (drop zone, files on the Pi with Print, Schedule, Copy to card and Delete, the card's listing), **Printer** (plug, preheat and cool, tram check with a bed map, firmware), **Schedule** (list with Cancel, and a count on the tab), **More** (Hue pair and plugs, `tram info`, "Read the mesh", with the last print's mesh and a coloured surface in the 3D view, added 2026-11-06, see Built: mesh heights and Built: mesh surface and the read after each print, versions). Below 760 px wide, the view goes on top and the panel below.
- **Drag and drop** a file anywhere on the window.
- **Tabs switch instantly,** highlight included, and each keeps its own scroll position. The scrolling area always keeps room for its scrollbar, so a tab that scrolls doesn't shift sideways, and the file and schedule lists are only rebuilt when they change. The user saw a glitch clicking between tabs on 2026-11-05: the highlight faded in over 150 ms after the content had already switched, the content jumped sideways by the scrollbar's width, and all tabs shared one scroll position. All three were fixed and checked from screencast frames in headless Chrome, not yet by the user.
- **A failed tram check** shows the bed map from above, the corner to change outlined, the range against the limit, and "Lower back-right: turn its knob counter-clockwise, seen from above", with **Check again** (`print --continue`) and **Cancel print**.
- **Confirmations:** printing, scheduling and tram checks need the **bed is clear** box ticked, which starts unticked every time and is never remembered. Print from the Pi or the card is a choice, as `--from` is required in the CLI. The card is only offered once the file is on it. Cancel print, cool during a print, power off, delete and cancelling a schedule ask first, with the dangerous button in red and a "Keep printing" style other choice.
- **Emergency stop,** decided by Claude as the design left it open: a round red button at the top right, always visible, **held for 0.6 seconds**, by mouse, touch or keyboard, so a stray click can't fire it, with nothing else to get through. It then shows what the Pi says was done, and if the plug isn't confirmed off, to use the printer's own switch.
- **When it can't go on,** a cover says so: "tramline gui has stopped" or "This tab isn't signed in".
- **Runs until Ctrl-C,** also decided by Claude: it doesn't stop when its tab closes.

### Trying it on the simulator

```sh
./sim-gui.sh [--minutes <n>] [--fail-tram] [--no-open] [--port <n>] [file.gcode ...]
```

Like `sim-web.sh`: a simulated printer on `127.0.0.1:18726` with a throwaway key, nothing on the Pi touched, and the files given uploaded first. `--fail-tram` gives a bed that fails its check, to see the "needs you" card. `--port` runs a second one beside another, such as one the user has open. The browser opens through `$BROWSER` with the real config folder put back, as `sim-web.sh` does. Ctrl-C ends the GUI and the simulator.

**Checking the page without a screen,** as was done on 2026-11-05: set `BROWSER` to a script that starts `google-chrome-stable --headless=new --remote-debugging-port=9333 --user-data-dir=<temp> --window-size=1440,900 --enable-unsafe-swiftshader "$1"` in the background, run `sim-gui.sh` with it, then drive the tab over the DevTools protocol, for instance with a short Node script using its built-in `WebSocket`: `Runtime.evaluate` to click and read, `Page.captureScreenshot`, `Input.dispatchMouseEvent` for the stop button's hold, `DOM.setFileInputFiles` for uploads, and `Runtime.exceptionThrown` and `Log.entryAdded` for errors. Settings such as `Emulation.setDeviceMetricsOverride` last only as long as the DevTools connection. The sign-in link only reaches the browser, so it never needs reading.

### Tested

- `go test`: each authentication rule, the route list both ways, the page and the Pi's status and model, uploads (sent once, the same file skipped, a changed one sent, bad names refused). The read-only page's test also checks it doesn't serve the GUI's files.
- **In headless Chrome on the simulator, 2026-11-05,** driven over the DevTools protocol, with screenshots: sign-in, power on, a tram check from the dialog with its live bed map and result, an upload during a print with the rename offer, a print streamed from the Pi with the 3D view and progress, cancelling it, a failed tram check with Check again and Cancel, scheduling with its list, cancel and tab count, copying to the card with Stop copy, Hue plugs, an error from `tram info` with the plug off, the stop button held briefly (nothing) and fully (stopped, plug off), a narrow window, and the GUI being stopped. No script errors. The read-only page was checked the same way after the split.

**Tram view, 2026-11-05:** the 3D view shows a tram check live, with the head moving, probe points and reading pins, see Built: tram check. Checked the same way, on the simulator only.

**Mesh view, 2026-11-05, later:** during a print's `G29 P1`, the 3D view shows the 5×5 mesh filling in and the head at the point UBL's order puts it, marked as an estimate, and the status card gives the point, see Built: mesh. Checked the same way, on the simulator only.

### Not proven

- Anything against the real Pi: the Hue Bridge's answers, and how a slow `GET /v1/files` or a real copy feels.
- A real browser, rather than headless Chrome: Firefox, KDE's default, and drag and drop from a file manager, which the driver can't do. Only the file picker path was driven.
- macOS: the build is made, but `open` and Safari haven't been tried.
- Uploading a large file over the real network, and the upload's progress as the Pi receives it.

