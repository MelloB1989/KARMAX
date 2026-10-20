#!/bin/bash
# Builds fleetctl for the machines people on the tailnet use, into the
# directory fleetd serves at /dl/ (fleet.yaml tailnet.dist). They install it
# with: curl -fsSL http://<fleetd host>:7878/install.sh | sh
# Run from anywhere: fleet/scripts/dist.sh [<dir>]  (default ~/.karmax/fleet/dist)
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:-$HOME/.karmax/fleet/dist}
mkdir -p "$out"
version=$(git describe --always --dirty 2>/dev/null || echo dev)
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os=${target%/*} arch=${target#*/}
	ext=; [[ $os == windows ]] && ext=.exe
	name=fleetctl-$os-$arch$ext
	# Built beside, then moved: a download in progress never reads half a file.
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X github.com/MelloB1989/karmax/fleet/internal/server.Version=$version" \
		-o "$out/.$name.new" ./cmd/fleetctl
	mv -f "$out/.$name.new" "$out/$name"
	echo "$out/$name"
done
