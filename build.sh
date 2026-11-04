#!/bin/sh
# Builds and tests Tramline inside a pinned Go container, so Go isn't needed on
# this machine. Networking is off, so nothing can be downloaded during a build.
#
#   ./build.sh          check, test, then build dist/tramline-linux-{arm64,amd64} and dist/tramline-darwin-arm64
#   ./build.sh test     check and test only
set -eu

# Official image, pinned by digest. To upgrade Go, change both on purpose.
IMAGE=golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb

cd "$(dirname "$0")"
mkdir -p dist .cache

VERSION=${TRAMLINE_VERSION:-dev-$(date -u +%Y%m%d%H%M%S)}

in_container() {
	docker run --rm --network none --user "$(id -u):$(id -g)" \
		--mount "type=bind,src=$PWD,dst=/src,readonly" \
		--mount "type=bind,src=$PWD/dist,dst=/src/dist" \
		--mount "type=bind,src=$PWD/.cache,dst=/cache" \
		-w /src \
		-e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/cache/go-build \
		-e GOPROXY=off -e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly \
		-e GOTELEMETRY=off -e CGO_ENABLED=0 \
		"$@"
}

in_container "$IMAGE" sh -euc '
	# Policy: standard library only. A third-party module must be a deliberate
	# change to this check, not something that arrives quietly.
	if [ -e go.sum ] || [ "$(go list -m all | wc -l)" != 1 ]; then
		echo "error: third-party modules are not allowed:" >&2
		go list -m all >&2
		exit 1
	fi
	unformatted=$(gofmt -l .)
	if [ -n "$unformatted" ]; then
		echo "error: not gofmt-formatted:" >&2
		echo "$unformatted" >&2
		exit 1
	fi
	go vet ./...
	GOOS=darwin GOARCH=arm64 go vet ./...
	go test -count=1 -timeout 120s ./...
'

[ "${1:-}" = test ] && exit 0

in_container -e VERSION="$VERSION" "$IMAGE" sh -euc '
	for target in linux/arm64 linux/amd64 darwin/arm64; do
		os=${target%/*} arch=${target#*/}
		GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
			-ldflags "-s -w -X main.version=$VERSION" \
			-o "dist/tramline-$os-$arch" .
	done
	cd dist
	sha256sum tramline-linux-arm64 tramline-linux-amd64 tramline-darwin-arm64 | tee SHA256SUMS
'
