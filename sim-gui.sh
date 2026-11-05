#!/bin/sh
# Tries tramline gui against the simulated printer, on this machine only. It needs no Pi
# and no printer: the server has a simulated printer and plug and no Hue settings, and
# everything it makes goes in a temporary folder, including a throwaway client key, so
# the real login isn't touched. Ctrl-C ends it and removes that.
#
#   ./sim-gui.sh [--minutes <n>] [--fail-tram] [--no-open] [--port <n>] [file.gcode ...]
#
#   --minutes <n>  how long a print from the Pi takes, default 10, with the first file's lines
#   --fail-tram    a bed that fails its tram check, to see the GUI ask for an adjustment
#   --no-open      don't open a browser, show the sign-in address instead
#   --port <n>     the simulated Pi's port, default 18726, to run two at once
#
# Files given are uploaded first, so there's something to print. Build first with ./build.sh.
set -eu

port=18726
minutes=10
open=1
level=--sim-level
files=

while [ $# -gt 0 ]; do
	case $1 in
	--minutes) minutes=${2:?--minutes needs a number}; shift ;;
	--fail-tram) level= ;;
	--no-open) open= ;;
	--port) port=${2:?--port needs a number}; shift ;;
	-*) echo "usage: $0 [--minutes <n>] [--fail-tram] [--no-open] [--port <n>] [file.gcode ...]" >&2; exit 2 ;;
	*)
		[ -r "$1" ] || { echo "can't read $1" >&2; exit 1; }
		files="$files
$1"
		;;
	esac
	shift
done

here=$(cd "$(dirname "$0")" && pwd)
case $(uname -s) in
Darwin) tramline=$here/dist/tramline-darwin-arm64 launcher=open ;;
*) tramline=$here/dist/tramline-linux-amd64 launcher=xdg-open ;;
esac
[ -x "$tramline" ] || { echo "no $tramline, run ./build.sh first" >&2; exit 1; }

temp=$(mktemp -d)
server=
cleanup() {
	[ -z "$server" ] || kill "$server" 2>/dev/null || true
	rm -rf "$temp"
}
trap cleanup EXIT
trap 'exit 0' INT TERM

# The client keeps its login and key here, not in the real ~/.config/tramline. The
# browser is started with the real value, as its profile and default are found through it.
real_config=${XDG_CONFIG_HOME-}
real_config_set=${XDG_CONFIG_HOME+1}
export XDG_CONFIG_HOME="$temp/client"
mkdir -p "$temp/data" "$XDG_CONFIG_HOME"
cat >"$temp/config.json" <<JSON
{"listen":"127.0.0.1:$port","data_dir":"$temp/data","serial":{"device":"/dev/null","baud":250000}}
JSON

# A stream's pace comes from each line's delay, set from the first file.
delay=0ms
first=$(printf '%s\n' "$files" | sed -n 2p)
if [ -n "$first" ]; then
	lines=$(grep -cvE '^[[:space:]]*(;|$)' "$first")
	delay=$((minutes * 60000000 / lines))us
fi

fingerprint() { grep -oE '[0-9a-f]{64}' | head -n 1; }
"$tramline" init --ip 127.0.0.1 --data "$temp/data" >/dev/null
"$tramline" allow "$("$tramline" keygen | fingerprint)" --name sim --data "$temp/data" >/dev/null
# shellcheck disable=SC2086
"$tramline" serve --sim $level --sim-line-delay "$delay" --sim-print-time "${minutes}m" \
	--config "$temp/config.json" >"$temp/server.log" 2>&1 &
server=$!

tries=0
until "$tramline" login "https://127.0.0.1:$port" "$("$tramline" fingerprint --data "$temp/data" | fingerprint)" >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 50 ] || { cat "$temp/server.log" >&2; echo "the server didn't start" >&2; exit 1; }
	kill -0 "$server" 2>/dev/null || { cat "$temp/server.log" >&2; exit 1; }
	sleep 0.2
done

printf '%s\n' "$files" | while IFS= read -r f; do
	[ -z "$f" ] || "$tramline" upload "$f" >/dev/null
done

# The GUI opens the browser through $BROWSER, with the real config folder put back.
cat >"$temp/browser" <<SH
#!/bin/sh
$(if [ -n "$real_config_set" ]; then echo "export XDG_CONFIG_HOME='$real_config'"; else echo "unset XDG_CONFIG_HOME"; fi)
exec ${BROWSER:-$launcher} "\$@"
SH
chmod +x "$temp/browser"

echo "Simulated printer running, nothing here touches the real one."
echo "To use the client on it, in another terminal:"
echo "  XDG_CONFIG_HOME=$XDG_CONFIG_HOME $tramline status"
echo "Server log: $temp/server.log"
echo "Ctrl-C ends it and removes all of this."
echo
if [ -n "$open" ]; then
	BROWSER=$temp/browser "$tramline" gui
else
	"$tramline" gui --no-open
fi
