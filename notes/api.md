# Commands and API

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

Port `8726` is proposed, which spells TRAM on a phone keypad. Not currently in use on the Pi.

| Client command | Endpoint | What it does |
|---|---|---|
| `tramline init --ip <address>` | none | On the Pi, once: create the server's certificate |
| `tramline fingerprint` | none | On the Pi: show the server's public fingerprint |
| `tramline allow <fingerprint>` | none | On the Pi: allow a client |
| `tramline revoke <fingerprint>` | none | On the Pi: remove a client |
| `tramline clients` | none | On the Pi: list allowed clients |
| `tramline serve` | none | Run the server, add `--sim` for a simulated printer |
| `tramline keygen` | none | On a client, once: create its key and show its fingerprint |
| `tramline login <url> <server-fingerprint>` | `GET /v1/status` | Check both certificates and save the connection |
| `tramline status` | `GET /v1/status` | Power, temperatures, job, current activity |
| `tramline gui [--no-open] [--port <n>]` | Those below, through a server on `127.0.0.1` | Open the full GUI in a browser, see Desktop GUI. Built, simulator only |
| `tramline web [--url]` | `http://<host>:8726/`, no certificate | Open the read-only web page in a browser, or just show its address. Built |
| `tramline watch` | `GET /v1/events` | Live status stream |
| `tramline power on\|off` | `POST /v1/power` | Plug control; off refuses while hot or printing |
| `tramline preheat [bed] [nozzle]` | `POST /v1/preheat` | Start heating |
| `tramline cool` | `POST /v1/cool` | Heaters off |
| `tramline tram --bed-clear [--inset <mm>] [--repeat <n>] [--json]` | `POST /v1/tram` | Probe corners and report. Exits 3 if out of tram |
| `tramline tram info` | `GET /v1/tram/info` | Fixed read-only list: `M118`, `M114`, `M851`, `M503`, `G29 W` |
| `tramline mesh [--raw] [--json]` | `GET /v1/mesh` | The mesh the printer holds now, from `M420 V1 T1`, which only prints. Nothing moves. Refused during a print, a tram check or a copy, and with the plug off. Run on the real printer 2026-11-06, see Built: mesh heights. Each print Tramline runs also reads it once after the print, into the job's `mesh_read` in `GET /v1/status` and `mesh-history.jsonl` |
| `tramline upload <path>` | `GET` and `PUT /v1/files/<name>` | Save on the Pi unless it's already there, nothing else. Built |
| `tramline copy <name> [--no-wait]`, `tramline copy --cancel` | `POST /v1/files/<name>/copy`, `POST /v1/copy/cancel` | Copy a file on the Pi to SD, which is also how to retry, or stop a copy. Built |
| `tramline files [--full] [--json]`, `tramline files rm <name>` | `GET /v1/files`, `DELETE /v1/files/<name>` | List files on the Pi and the card with their SHA-256 sums, see Checksums under Upload, or delete one, with its copy on the card. Built |
| `tramline print <file> --from pi\|sd --bed-clear` | `POST /v1/print` | Turn on if needed, heat, check tram, print, turn off once cool; refused without the bed-clear confirmation. Built |
| `tramline cancel` | `POST /v1/cancel` | Cancel with `M524`. Built, simulator only |
| `tramline estop` | `POST /v1/estop` | `M112`, then cut the plug. Built, simulator only |
| `tramline schedule <file> --from pi\|sd --at "YYYY-MM-DD HH:MM" --bed-clear` | `POST /v1/schedule` | Check tram now, power off, print at that time if tram still passes. Built, simulator only |
| `tramline schedule list [--json]`, `tramline schedule cancel <id>` | `GET /v1/schedule`, `POST /v1/schedule/<id>/cancel` | List them, or cancel one that hasn't started printing. Built, simulator only |
| `tramline hue pair` | `POST /v1/hue/pair` | Pair after the bridge's button is pressed |
| `tramline hue plugs` | `GET /v1/hue/plugs` | List names and UUIDs, for filling in the config |
| `tramline update <binary>` | `PUT /v1/update` | Replace the server binary |

Long jobs return straight away with a job ID, and clients follow them through `status` or `watch`. An upload arrives at the Pi quickly over the network, so the slow serial copy carries on even if the client disconnects, and a failed copy can retry without re-sending the file.
