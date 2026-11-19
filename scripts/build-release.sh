#!/bin/bash
# Builds the release archives and their SHA256SUMS into a directory.
#
# usage: scripts/build-release.sh [out dir] [version]
#
# The version defaults to the git tag (v0.2.0 builds 0.2.0). The archives are
# reproducible: the same source and Go toolchain give the same bytes, with the
# files dated to the last commit, or to SOURCE_DATE_EPOCH when it is set.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=$(mkdir -p "${1:-dist}" && cd "${1:-dist}" && pwd)
VERSION=${2:-$(git describe --tags --always --dirty)}
VERSION=${VERSION#v}
MTIME=@${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}
TARGETS="darwin/amd64 darwin/arm64 linux/amd64 windows/amd64"
FILES="README.md LICENSE"

export CGO_ENABLED=0 TZ=UTC
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

go version
for target in $TARGETS; do
  IFS=/ read -r os arch <<< "$target"
  name=loka_${VERSION}_${os}_${arch}
  bin=loka
  [ "$os" = windows ] && bin=$bin.exe
  dir=$STAGE/$name
  mkdir "$dir"

  GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-w -s -X main.version=$VERSION" -o "$dir/$bin" .
  # run the build that suits this computer before packing it
  if [ "$target" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]; then
    got=$("$dir/$bin" --version)
    [ "$got" = "loka $VERSION" ] || { echo "$name: --version printed '$got'" >&2; exit 1; }
    "$dir/$bin" -h 2> /dev/null || { echo "$name: -h failed" >&2; exit 1; }
    echo "$name: --version and -h work"
  fi
  # shellcheck disable=SC2086
  cp $FILES "$dir/"
  # the same modes and dates whatever the umask and checkout time
  chmod 0644 "$dir"/*
  chmod 0755 "$dir/$bin"
  touch -d "$MTIME" "$dir"/*

  rm -f "$OUT/$name".{tar.gz,zip}
  if [ "$os" = windows ]; then
    # shellcheck disable=SC2086
    (cd "$dir" && zip -q -X -9 "$OUT/$name.zip" "$bin" $FILES)
  else
    # shellcheck disable=SC2086
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$MTIME" \
      -C "$dir" -cf - "$bin" $FILES | gzip -n -9 > "$OUT/$name.tar.gz"
  fi
  echo "$name"
done

cd "$OUT"
sha256sum loka_"${VERSION}"_* > SHA256SUMS
cat SHA256SUMS
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
fi
