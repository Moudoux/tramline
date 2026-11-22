# Goal and hardware

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Goal

1. Turn the printer on if it is off.
2. Preheat if needed.
3. Check bed tram, and pause for manual adjustment if needed.
4. Upload a print and start it.

This is a lightweight alternative to OctoPrint, which the user finds heavy. The full workflow becomes SolidCraft design, then slicing in `~/Applications/Solidcraft/cura/`, then Tramline.

## Hardware

| Item | Details | Source |
|---|---|---|
| Printer | Creality Ender 3 V2, stock Bowden, 0.4 mm nozzle | User |
| Board and firmware | 4.2.2 board, `Marlin 2.1.3 MRiscoC Ender3V2-422-BLTUBL-MPC`, built 17 Dec 2023 | M115 |
| Probe and levelling | Touch probe, UBL | User and M115 |
| Host | Raspberry Pi 4 Model B Rev 1.1, 4 GB RAM, aarch64, Debian 12 | SSH |
| Pi role | Also runs Docker containers and Tailscale | SSH |
| Pi boot disk | Sabrent USB SSD on the USB 3 bus, holds `/` and `/boot/firmware` | SSH |
| Power control | Philips Hue smart plug, with a Hue Bridge | User |
| Camera | None | User |
| Filament | PETG | User |
| Bed surface | Thin flexible plate made for PETG, not glass | User |

## Verified: connection

Tested 2026-11-02 from this machine.

- `ssh raspberrypi` logs in as `pi` without a password, and `pi` is in `dialout`.
- Serial path: `/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0`, which pointed to `/dev/ttyUSB0`.
- The USB chip is a CH340 (`1a86:7523`) using the `ch341` driver, on USB port `1-1.3`.
- The serial device disappears cleanly when the printer loses power, and returns when it comes back, tested with both the plug and the printer's switch.
- The link works at **250000 baud**. It answered on the first baud rate tried, so 115200 was not tested.
- Temperatures read: nozzle 21.96°C, bed 22.49°C, heaters off.
- `vcgencmd get_throttled` returned `0x0` before and after the serial test, with the printer on.
- Opening the port does not appear to restart the board, tested once with the printer idle, see below. It hasn't been tested during a print.
- Opening the port let in 6 garbage bytes, which gave a harmless `Unknown command`. Send a newline first to clear these.


### Read-only status through the server

Stage 5, tested 2026-11-02 at 20:35 to 20:54 GMT, with logs in `test-logs/2026-11-02-stage5/`.

| Test | Result |
|---|---|
| Idle for about 11 minutes, 600 status samples | Plug on throughout. Connected after about 8 seconds, temperatures 2 seconds later, then no dropouts |
| Temperatures, heaters off | Kept updating. Nozzle about 20.7 to 21.4°C, bed about 20.5 to 20.8°C |
| `sudo systemctl restart tramline` with the printer on | Reconnected 0.56 seconds after the server started. Screen didn't change, and no `start` line |
| `power off`, then about 6 minutes' watch | USB dropped 3.1 seconds later, server logged the disconnect. Ping, login and `sync` fine, `throttled=0x0`, no failed units |

- **Board startup:** after power-on the board answers nothing until about 6 seconds after USB appears. See the 2026-11-03 update under Heating for why its late reply was logged as unprompted text.
- **`start` line:** it wasn't seen on power-on, probably because it's sent before USB is up, though that's unconfirmed. So its absence doesn't prove on its own that the board didn't restart. The restart result rests on that together with the screen and the fast reconnect.
- **Not tested:** pulling the USB cable with the printer on, which the user chose to skip, and reconnecting during a print.

### M115 capabilities

Present: `BINARY_FILE_TRANSFER`, `EEPROM`, `VOLUMETRIC`, `AUTOREPORT_TEMP`, `PRINT_JOB`, `AUTOLEVEL`, `RUNOUT`, `Z_PROBE`, `LEVELING_DATA`, `BUILD_PERCENT`, `EMERGENCY_PARSER`, `HOST_ACTION_COMMANDS`, `PROMPT_SUPPORT`, `SDCARD`, `SD_WRITE`, `LONG_FILENAME`, `LFN_WRITE`, `CUSTOM_FIRMWARE_UPLOAD`, `EXTENDED_M20`, `THERMAL_PROTECTION`, `ARCS`, `BABYSTEPPING`.

Absent: `SERIAL_XON_XOFF`, `AUTOREPORT_POS`, `PROGRESS`, `SOFTWARE_POWER`, `TOGGLE_LIGHTS`, `MULTI_VOLUME`, `REPEAT`, `AUTOREPORT_SD_STATUS`, `MOTION_MODES`, `MEATPACK`, `CONFIG_EXPORT`.

Reported area: full `248 x 231 x 250`, work `230 x 230 x 250`. These match the firmware's own defaults (`X_BED_SIZE 230`, `X_MAX_POS 248`, `Y_MAX_POS 231`), and the user has not changed them. The Cura profile separately slices for 220 x 220, so this needs no action.

## The USB cable

The cable is modified so the printer can't be powered from the Pi over USB. The user opened the jacket and cut only the red +5V wire. The [firmware wiki](https://github.com/mriscoc/Ender3V2S1/wiki/Octoprint) also recommends disabling +5V.

- With the printer off and the cable connected, the user confirmed the screen is fully black.
- With the printer on, data works, as shown above.

Software switching of USB power is not an option. The Pi 4's ports switch [as one group](https://github.com/mvp/uhubctl), which would also cut power to the boot SSD.

**Never run `uhubctl` against this Pi's USB hubs.**
