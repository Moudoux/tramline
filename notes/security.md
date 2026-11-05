# Security

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

With SSH gone, Tramline must provide its own access control and encryption. Anyone who can send commands can heat the printer or start it moving, and the Pi's firewall is off (`ufw` inactive).

**Mutual TLS, chosen by the user over a shared token.** Each side has its own key pair and self-signed certificate, and trusts the other by SHA-256 fingerprint. There's no certificate authority.

- **Server identity:** `tramline init` creates the server's key. The client gets its fingerprint at login, not from the first connection, and refuses any other server.
- **Client identity:** `tramline keygen` creates the key on the client, so a private key never has to move between machines. TLS proves the client holds it, and it's never sent.
- **Allow list:** `tramline allow <fingerprint>` adds a client to `/var/lib/tramline/clients.allow`, which holds only public fingerprints. It's read for every connection and request, so allowing or revoking needs no restart, and revoking also cuts off open connections. A missing or corrupted list denies everyone.
- **TLS 1.3 only.** Every API endpoint needs an allowed certificate. Plain HTTP on the same port reaches only the read-only web page, which the user asked for on 2026-11-04, see Web page.
- **Listening addresses:** the LAN and Tailscale, decided by the user on 2026-11-04, which replaces LAN only. The server listens on the addresses of each interface in `listen_interfaces`, which are `eth0` and `tailscale0`. It doesn't listen on `eth0.2` or the Docker bridges, and "all interfaces" stays refused. Containers could still route to it, which is why client certificates are required. See Listening by interface.
- **Limits enforced by the server,** not the client: see Safety rules.
- **Files:** uploaded names are restricted to a safe character set and length, with a maximum upload size.

**What this doesn't solve:** `client.key` is a plain file with owner-only permissions. Claude Code's global settings deny its file tools access, but shell commands aren't fully covered, so anything running as the user could still read it. Whoever copies it can control Tramline until it's revoked. A key store that needs unlocking would stop Claude running commands unattended.
