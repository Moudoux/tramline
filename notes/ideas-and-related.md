# Ideas parked and related files

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Ideas parked

**Motorised knobs.** Pi-controlled motors could turn the knobs without the user. It's feasible, but advised against for now because the mesh already covers normal tilt and someone still has to remove prints. The hard parts:

- knobs move along their screws as they turn, so couplings must allow for this
- under-bed clearance across the Y travel hasn't been measured
- PETG softens near the 80°C bed temperature
- small printed gears are a known weak spot on this printer
- extra moving mass and flexing cables on the Y axis
- no position feedback, so software limits are essential

Log real drift first, then try one motor on one corner if it's justified. Solid bed spacers may be a simpler fix.

**Other later ideas:** binary upload, a camera, an MCP server wrapping the API.

## Related files

- `~/Applications/Solidcraft/cura/`: slicing scripts, PETG profile and start G-code
- `~/Applications/Solidcraft/cura/start_gcode_PETG.txt`: the start sequence referred to above, which keeps `G29 P1`, `G29 A` and `C108`
