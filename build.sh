#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

# set by each crosscompile, and the same for all the binaries of a run: names the sums file
version=""

crosscompile () {
    local name
    if [ "$2" == "windows" ]; then
        name="$1.exe"
    else
        name="$1"
    fi
    echo "Compiling ${1} for ${2}/${3}..."
    # no cgo: static binaries, independent of the libc of the machine building them
    CGO_ENABLED=0 GOOS="$2" GOARCH="$3" go build -trimpath -ldflags="-s" -o "$name" "./cmd/${1}"
    # the version Go stamped, which --version reports (a tag, or a pseudo-version, "+dirty" for
    # uncommitted changes), rather than git describe, which only agrees with it on a clean tag
    version=$(go version -m "$name" | awk '$1 == "mod" { print $3 }')
    zip -9 "${1}_${version}_${2}_${3}.zip" "$name"
    rm "$name"
    echo
}

# Windows: rat and rat-tool only, ratd needs tmux
## amd64
crosscompile 'rat' 'windows' 'amd64'
crosscompile 'rat-tool' 'windows' 'amd64'
## arm64
crosscompile 'rat' 'windows' 'arm64'
crosscompile 'rat-tool' 'windows' 'arm64'

# Linux
## amd64
crosscompile 'ratd' 'linux' 'amd64'
crosscompile 'rat' 'linux' 'amd64'
crosscompile 'rat-tool' 'linux' 'amd64'
## arm64
crosscompile 'ratd' 'linux' 'arm64'
crosscompile 'rat' 'linux' 'arm64'
crosscompile 'rat-tool' 'linux' 'arm64'

# macOS
## Intel (amd64)
crosscompile 'ratd' 'darwin' 'amd64'
crosscompile 'rat' 'darwin' 'amd64'
crosscompile 'rat-tool' 'darwin' 'amd64'
## Apple Silicon (arm64)
crosscompile 'ratd' 'darwin' 'arm64'
crosscompile 'rat' 'darwin' 'arm64'
crosscompile 'rat-tool' 'darwin' 'arm64'

# Compute hashes
shasum -a 256 rat*_"${version}"_*.zip > "rat_${version}_SHA256SUMS"
