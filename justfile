# Tramline tasks. Run `just` in this folder to list them, or `just <recipe>`
# the repository root, to list or run them.

bindir := env("TRAMLINE_BINDIR", home_directory() / ".local/bin")

goos := if os() == "macos" { "darwin" } else { os() }
goarch := if arch() == "aarch64" { "arm64" } else if arch() == "x86_64" { "amd64" } else { arch() }
binary := "dist/tramline-" + goos + "-" + goarch

default:
    @just --list

# Check, test and build every platform into dist/
build:
    ./build.sh

# Check and test only
test:
    ./build.sh test

# Build, then install this machine's binary as `tramline` in ~/.local/bin (or $TRAMLINE_BINDIR)
install: build
    @test -f {{binary}} || { echo "error: no build for {{goos}}/{{goarch}}" >&2; exit 1; }
    install -d {{bindir}}
    install -m 755 {{binary}} {{bindir}}/tramline
    @echo "installed {{bindir}}/tramline"

# Remove the installed binary
uninstall:
    rm -f {{bindir}}/tramline

# Build the Pi's server package, dist/tramline_<VERSION>_arm64.deb (needs dpkg-deb)
deb:
    TRAMLINE_VERSION=$(cat VERSION) TRAMLINE_TARGETS=linux/arm64 ./build.sh
    packaging/build-deb.sh

# Build this machine's Arch package in packaging/arch/, without installing it
arch-pkg:
    cd packaging/arch && makepkg -f
