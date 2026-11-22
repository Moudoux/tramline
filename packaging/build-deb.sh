#!/bin/sh
# Packages the Pi's server as dist/tramline_<version>_arm64.deb, from the
# binary ./build.sh made. Run it through `just deb`, which builds that first
# with the version from VERSION. Needs dpkg-deb (Arch: the dpkg package).
#
# The package installs the binary, the service, its user, data folder and
# serial driver, a default config if there's none, and enables the service.
set -eu

cd "$(dirname "$0")/.."
version=$(cat VERSION)
binary=dist/tramline-linux-arm64
out=dist/tramline_${version}_arm64.deb

command -v dpkg-deb >/dev/null || {
	echo "error: dpkg-deb is missing (Arch: the dpkg package)" >&2
	exit 1
}
[ -f "$binary" ] || {
	echo "error: no $binary, run 'just deb'" >&2
	exit 1
}
# The binary must be this version, not an older dev build left in dist/. It's
# run under qemu where that's installed, or else searched for the string.
if command -v qemu-aarch64-static >/dev/null; then
	built=$(qemu-aarch64-static "$binary" version)
	[ "$built" = "$version" ] || {
		echo "error: $binary is $built, not $version, run 'just deb'" >&2
		exit 1
	}
elif ! grep -aqF "$version" "$binary"; then
	echo "error: $binary wasn't built as $version, run 'just deb'" >&2
	exit 1
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
root=$stage/root
deb=packaging/deb

install -Dm755 "$binary" "$root/usr/bin/tramline"
install -Dm644 "$deb/tramline.service" "$root/usr/lib/systemd/system/tramline.service"
install -Dm644 "$deb/sysusers.conf" "$root/usr/lib/sysusers.d/tramline.conf"
install -Dm644 "$deb/tmpfiles.conf" "$root/usr/lib/tmpfiles.d/tramline.conf"
install -Dm644 "$deb/modules-load.conf" "$root/usr/lib/modules-load.d/tramline.conf"
install -Dm644 "$deb/config.json" "$root/usr/share/tramline/config.json"
install -Dm644 README.md "$root/usr/share/doc/tramline/README.md"

install -d "$root/DEBIAN"
for script in postinst prerm postrm; do
	install -m755 "$deb/$script" "$root/DEBIAN/$script"
done

size=$(du -sk --exclude=DEBIAN "$root" | cut -f1)
cat >"$root/DEBIAN/control" <<EOF
Package: tramline
Version: $version
Architecture: arm64
Maintainer: Moudoux <Moudoux@users.noreply.github.com>
Installed-Size: $size
Depends: systemd, kmod
Section: misc
Priority: optional
Homepage: https://github.com/Moudoux/tramline
Description: Print server for an Ender 3 V2 on a Raspberry Pi
 Switches the printer through a Hue plug, checks the bed's tram, uploads
 and prints G-code over USB, and serves a read-only status page.
EOF

dpkg-deb --root-owner-group -Zxz --build "$root" "$out"
dpkg-deb --info "$out"
dpkg-deb --contents "$out"
sha256sum "$out"
