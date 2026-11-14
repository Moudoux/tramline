# Test plan and open questions

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Test plan

Each stage must pass before moving on. **Hue comes first, so Claude can power the printer for later tests without the user being at the plug. Claude still asks the user before every test that involves the printer.**

0. Controlled power-off with the Pi connected. Passed three times on 2026-11-02, see above. Keep capturing kernel logs during later power testing.
1. Build against a simulator and fake bridge, including `busy:`, garbage bytes and disconnects, resends and rejected certificates. Done.
2. User installs the update and allows this machine's fingerprint. Log in. Done 2026-11-02.
3. Pair with the real bridge, find the plug's UUID, and put it in the config. Done 2026-11-02.
4. Plug control from `tramline power`, with the printer idle, and confirm the plug state. Passed 2026-11-02, three on and three off.
5. Read-only printer status through the real USB adapter, including power cycling and reconnecting, and whether reconnecting restarts the board. Passed 2026-11-02, with the printer idle.
6. Heating, with the user watching. Passed 2026-11-03 at low temperatures: heat, cool, power-off refusals and the 30-minute timeout.
7. `G30` probing, with the user watching, compared against the LCD wizard, including probe points and repeatability. Passed 2026-11-03, hot and cold, and the wizard agrees in both cases.
8. Small SD upload, verified with `M20`, and timed for a real speed figure. Passed 2026-11-03: a sliced file of 15,264 lines in 31 s at 14 KB/s, with resends working. Other failure handling is still simulator only.
9. Start a small test print. Check for an LCD confirmation prompt, and confirm cancelling works. Passed 2026-11-03: no prompt, a good print, and cancelling works hot. Test files in `test-logs/2026-11-03-print/`.
10. Printing by streaming. Passed 2026-11-04, including a 63-minute job, cancels, the LCD's Stop, `M85` firing and a restart. A link dropping mid-print is simulator only.
11. Full flow, end to end.

## Open questions

1. Is the Pi and the Hue plug stable over many power cycles?
2. ~~Does a remotely started SD print wait for confirmation on the LCD?~~ No, tested 2026-11-03 with a cold file. See Print design.
3. Is `G30` available and behaving the same way on the Dec 2023 build?
4. ~~What are this printer's tramming points?~~ 25 and 205, the mesh limits in `M503`'s `C29` line. See Tram check.
5. ~~What's the real upload speed?~~ 14 KB/s with a 120-byte window, on one small sliced file. See Upload.
7. What caused the one resend at a safe window? Every command will get a line number and checksum either way, decided by the user.
6. Should the firmware be updated, or left on the current build?

## To do outside the code

- The user will review Hue rooms and routines for "Light plug" later.
