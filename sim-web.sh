#!/bin/sh
# Tries the web page against the simulated printer, on this machine only. It needs no
# Pi and no printer: the server has a simulated printer and plug and no Hue settings,
# and everything it makes goes in a temporary folder, including a throwaway client key,
# so the real login isn't touched. Ctrl-C ends it and removes that.
#
#   ./sim-web.sh [--sd] [--minutes <n>] [--no-open] [--lan] [file.gcode]
#
#   --sd           print from the simulated card, instead of streaming
#   --minutes <n>  how long the print takes, default 10
#   --no-open      don't open a browser, just show the address
#   --lan          listen on this machine's network address, not 127.0.0.1, to try it on a phone
#
# With no file, the page stays idle. Build first with ./build.sh.
set -eu

port=18726
sd=
minutes=10
open=1
listen=127.0.0.1
file=
while [ $# -gt 0 ]; do
	case $1 in
	--sd) sd=1 ;;
	--minutes) minutes=${2:?--minutes needs a number}; shift ;;
	--no-open) open= ;;
	--lan) listen=$(ip route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p') ;;
	-*) echo "usage: $0 [--sd] [--minutes <n>] [--no-open] [--lan] [file.gcode]" >&2; exit 2 ;;
	*) file=$1 ;;
	esac
	shift
done
[ -z "$file" ] || [ -r "$file" ] || { echo "can't read $file" >&2; exit 1; }

[ -n "$listen" ] || { echo "couldn't find this machine's network address" >&2; exit 1; }
url=https://$listen:$port

here=$(cd "$(dirname "$0")" && pwd)
tramline=$here/dist/tramline-linux-amd64
[ -x "$tramline" ] || { echo "no $tramline, run ./build.sh first" >&2; exit 1; }

temp=$(mktemp -d)
server=
cleanup() {
	[ -z "$server" ] || kill "$server" 2>/dev/null || true
	rm -rf "$temp"
}
trap cleanup EXIT
trap 'exit 0' INT TERM

# The client keeps its login and key here, not in the real ~/.config/tramline. The real
# value is kept for the browser, whose profile and default are found through it.
real_config=${XDG_CONFIG_HOME-}
real_config_set=${XDG_CONFIG_HOME+1}
export XDG_CONFIG_HOME="$temp/client"
mkdir -p "$temp/data" "$XDG_CONFIG_HOME"
cat >"$temp/config.json" <<JSON
{"listen":"$listen:$port","data_dir":"$temp/data","serial":{"device":"/dev/null","baud":250000}}
JSON

# A stream's pace comes from each line's delay. A copy to the card would take that too,
# so a print from the card takes its time from --sim-print-time instead.
delay=0ms
if [ -n "$file" ] && [ -z "$sd" ]; then
	lines=$(grep -cvE '^[[:space:]]*(;|$)' "$file")
	delay=$((minutes * 60000000 / lines))us
fi

fingerprint() { grep -oE '[0-9a-f]{64}' | head -n 1; }
"$tramline" init --ip 127.0.0.1 --data "$temp/data" >/dev/null
"$tramline" allow "$("$tramline" keygen | fingerprint)" --name sim --data "$temp/data" >/dev/null
"$tramline" serve --sim --sim-level --sim-line-delay "$delay" --sim-print-time "${minutes}m" \
	--config "$temp/config.json" >"$temp/server.log" 2>&1 &
server=$!

tries=0
until "$tramline" login "$url" "$("$tramline" fingerprint --data "$temp/data" | fingerprint)" >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 50 ] || { cat "$temp/server.log" >&2; echo "the server didn't start" >&2; exit 1; }
	kill -0 "$server" 2>/dev/null || { cat "$temp/server.log" >&2; exit 1; }
	sleep 0.2
done

"$tramline" power on >/dev/null
if [ -n "$file" ]; then
	tries=0
	until "$tramline" status --json | grep -Eq '"connected": ?true'; do
		tries=$((tries + 1))
		[ "$tries" -lt 50 ] || { echo "the simulated printer didn't connect" >&2; exit 1; }
		sleep 0.2
	done
	# Uploads go by the file's own name, so it has to be one the server accepts.
	name=$(basename "$file")
	if [ -n "$sd" ]; then
		"$tramline" upload "$file" --sd
	else
		"$tramline" upload "$file" --pi-only
	fi
	"$tramline" print "$name" --bed-clear
fi

address=$("$tramline" web --url)
echo "$address"
if [ -n "$open" ]; then
	(
		if [ -n "$real_config_set" ]; then
			export XDG_CONFIG_HOME="$real_config"
		else
			unset XDG_CONFIG_HOME
		fi
		exec xdg-open "$address"
	) >/dev/null 2>&1 &
fi
if [ "$listen" != 127.0.0.1 ]; then
	echo "Open that on a phone on the same network."
fi
echo
echo "Simulated printer running, nothing here touches the real one."
echo "To use the client on it, in another terminal:"
echo "  XDG_CONFIG_HOME=$XDG_CONFIG_HOME $tramline status"
echo "Server log: $temp/server.log"
echo "Ctrl-C ends it and removes all of this."
wait "$server"
