# Notifications

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

Built 2026-11-25 at the user's request: the Pi sends notifications to the user's Switchboard server, following Switchboard's own guide, `~/Documents/Code/notifications/SENDING-NOTIFICATIONS.md`. The CLI manages them with `tramline notify`, and the GUI's More tab has the same settings. **`go test`, headless Chrome and the CLI against `sim-gui.sh`, with a fake Switchboard, only. Not installed, and never sent to the real Switchboard.**

## Design

- **The Pi sends them,** as it's the one that sees a print end, often with no client running. Code: `internal/notify` (settings, queue, sending) and `internal/server/notify.go` (the API and what each event says).
- **Settings live in the data folder, not `/etc/tramline/config.json`,** so a client can change them without sudo. `notify.json` (mode 640) holds `enabled`, `url`, `link` and `events`. The token is in its own file, `switchboard.token`, mode 600, like `hue.key`. `/var/lib/tramline` is 700 anyway. A `notify.json` that can't be read is reported as `problem` in `GET /v1/notify` and the log, and notifications stay off until they're saved again. It doesn't stop the server.
- **The token never comes back.** `GET /v1/notify` says `token_set` only. The CLI reads it from stdin, never the command line: on a terminal it asks with echo off (`stty -echo`, put back on Ctrl-C too), and refuses to ask if that fails. The GUI's field is a password field, cleared after each save. Nothing logs it, and Switchboard errors are shown without it.
- **Sending never holds anything up.** Hooks only queue (64 deep; a full queue drops and counts). One goroutine sends, with a 10-second limit, retrying 5xx, network errors and timeouts after 1, 2, 4 and 8 seconds, as the guide says. A 4xx isn't retried. Redirects aren't followed, so the token can't be carried elsewhere. `last` in `GET /v1/notify` is the latest result, which `tramline notify` and the GUI show.
- **On and off:** `enabled` switches every event; each event can be on or off as well, all on by default. Turning on needs an address and a token, and removing either turns them off. `notify test` sends even while off, so the address and token can be tried first. The GUI's "Save and test" saves the form, then tests.
- **`link`** is an optional URL put in each notification as `url`, such as the read-only page. The server can't work out its own address, as it follows interfaces.
- **`data`** has `host` (the Pi's hostname), `event`, and for prints `job_id`, `file`, `from`, `state`, `schedule_id`, `sha256` and `seconds`, so Switchboard's routes can filter. Titles stay the same for the same outcome, for Switchboard's dedupe, as the guide asks. Tags are `tramline` plus `print`, `schedule` or `problem` and the state.

## Events

| Event | Sent when | Title | Priority |
|---|---|---|---|
| `print_finished` | A print ends `finished` | Print finished | 3 |
| `print_failed` | A print ends `failed` or `unknown`, including the tram check when scheduling | Print failed, Print may not have finished, Scheduled print's tram check failed | 4 |
| `print_cancelled` | A print ends `cancelled`: from the CLI or GUI, the LCD, `cool`, or a scheduled print's check failing at its time | Print cancelled, Scheduled print cancelled | 3, or 2 for "cancelled on request" |
| `needs_adjusting` | A print's tram check fails and it waits for `print --continue` (a scheduled print at its time cancels instead) | Bed needs adjusting, with the corner and which way | 4 |
| `schedule_ended` | A scheduled print ends without its job reporting it: missed, the bed used since, the printer busy, power-on failing, the job lost in a restart, its file started from the LCD. Not a cancel on request | Scheduled print didn't start, Scheduled print missed | 4 |
| `problem` | `heat_problem` changes to a new problem, or the auto-off gives up with the plug on after 30 minutes | Printer problem, Printer left on | 5 if the heaters are named, else 4 |

A check made when scheduling that passes sends nothing, as the user is there. A schedule ended by its print job goes through `closeScheduleLocked`, which sends nothing, so one event never sends twice.

## Tested

- `internal/notify`: saving and reloading, the token's file and mode, the token absent from the view and `notify.json`, refused updates (bad URLs, a link that isn't http, bad tokens, unknown events, turning on unset), a broken file, sending with the bearer token, the chosen events only, retries on 5xx and none on 401, a test while off, no redirect followed, cutting long text on a character boundary.
- `internal/server`: the handlers, a print on the simulator finishing (one notification), a failed check asking for the knobs then a cancel on request at priority 2, a missed schedule sending and a schedule cancelled on request not.
- 2026-11-25, `dist/` `dev-20261125173246` on `sim-gui.sh` with a fake Switchboard on `127.0.0.1`: in headless Chrome, the More tab's form, refusing to turn on unset, Save and test, the token field clearing, the last send, Remove the token, a phone-width window, no script errors. The CLI: `notify`, `set`, `token` from a pipe and from a prompt in a pseudo-terminal (typing not shown), bad input, `on`, `test`, `--json` without the token, the files' modes.

## Not proven

- The real Switchboard, and its routes and dedupe.
- Any event from the real printer. The first real print after installing would show `print_finished`.
- `problem` on the Pi: it only fires when something has gone wrong with the heaters or the plug.
- The prompt on macOS, where `stty` should work the same.

## To set it up on the Pi

After installing a build with this in, from the desktop: `tramline notify set --url <Switchboard's address> --link http://raspberrypi:8726/`, then `tramline notify token` (the user types or pipes the token: Claude never reads it), `tramline notify test`, `tramline notify on`. Switchboard's address as the Pi sees it: from the Pi, not from a container, so not `http://switchboard:8080`.
