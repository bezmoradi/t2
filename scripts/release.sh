#!/usr/bin/env bash
set -euo pipefail

all_targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
macos_min=13.0

usage() {
	echo "usage: scripts/release.sh VERSION [OUTDIR [TARGET...]]" >&2
	echo "  VERSION is internal/version/version.go's VERSION without the v; OUTDIR defaults to dist" >&2
	echo "  TARGETs are from: $all_targets (default: all, which also writes install.sh and SHA256SUMS)" >&2
	echo "  darwin targets build with cgo and need macOS with the Xcode Command Line Tools; linux targets build without cgo anywhere" >&2
	exit 2
}

[ $# -ge 1 ] || usage
version=$1
out=${2:-dist}
shift $(($# >= 2 ? 2 : 1))
targets=${*:-$all_targets}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "release: invalid version $version" >&2; usage; }
for t in $targets; do
	case " $all_targets " in
	*" $t "*) ;;
	*) echo "release: unknown target $t" >&2; usage ;;
	esac
	if [ "${t%/*}" = darwin ] && [ "$(uname -s)" != Darwin ]; then
		echo "release: $t builds with cgo against macOS's frameworks, so it needs a macOS host" >&2
		exit 1
	fi
done

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

source_version=$(sed -n 's/^const VERSION = "v\([^"]*\)"$/\1/p' internal/version/version.go)
if [ "$source_version" != "$version" ]; then
	echo "release: internal/version/version.go says VERSION is v$source_version, not v$version; change it there first" >&2
	exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
	echo "release: the working tree has uncommitted changes; commit or stash them first" >&2
	exit 1
fi
commit=$(git rev-parse HEAD)

if [ -e "$out" ]; then
	echo "release: $out already exists; remove it or choose another directory" >&2
	exit 1
fi
mkdir -p "$out"
out=$(cd "$out" && pwd)

if command -v sha256sum >/dev/null 2>&1; then
	sum() { sha256sum "$@"; }
else
	sum() { shasum -a 256 "$@"; }
fi

goroot=$(go env GOROOT)
golicense=""
for f in "$goroot/LICENSE" "$goroot/../LICENSE"; do
	if [ -f "$f" ]; then
		golicense=$f
		break
	fi
done
if [ -z "$golicense" ]; then
	echo "release: the Go toolchain's LICENSE wasn't found near $goroot" >&2
	exit 1
fi

cgo_for() {
	if [ "${1%/*}" = darwin ]; then echo 1; else echo 0; fi
}

notices="$out/t2-$version-NOTICES.txt"
{
	echo "T2 $version, built from commit $commit with $(go version | cut -d' ' -f3)"
	echo
	echo "==== T2 ===="
	cat LICENSE
	echo
	echo "==== Go standard library ($(go version | cut -d' ' -f3)) ===="
	cat "$golicense"
	for t in $all_targets; do
		CGO_ENABLED=$(cgo_for "$t") GOOS=${t%/*} GOARCH=${t#*/} go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/t2
	done | sort -u |
		while read -r path ver dir; do
			found=0
			for f in "$dir"/LICENSE* "$dir"/NOTICE* "$dir"/COPYING*; do
				[ -f "$f" ] || continue
				echo
				echo "==== $path $ver: $(basename "$f") ===="
				cat "$f"
				found=1
			done
			if [ $found -eq 0 ]; then
				echo "release: no license file found for $path $ver" >&2
				exit 1
			fi
		done
} > "$notices"
echo "wrote $notices"

stage="$out/.stage"
trap 'rm -rf "$stage"' EXIT

for t in $targets; do
	os=${t%/*}
	arch=${t#*/}
	name="t2-$version-$os-$arch"
	pkg="$stage/$name"
	mkdir -p "$pkg"
	CGO_ENABLED=$(cgo_for "$t") MACOSX_DEPLOYMENT_TARGET=$macos_min GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid=" -o "$pkg/t2" ./cmd/t2
	cp LICENSE "$pkg/LICENSE"
	cp "$notices" "$pkg/NOTICES.txt"
	(cd "$pkg" && sum LICENSE NOTICES.txt t2 > "$stage/SHA256SUMS" && mv "$stage/SHA256SUMS" SHA256SUMS)
	COPYFILE_DISABLE=1 tar -C "$stage" --no-xattrs -czf "$out/$name.tar.gz" "$name"
	echo "built $out/$name.tar.gz"
done
rm -rf "$stage"
trap - EXIT

if [ "$targets" = "$all_targets" ]; then
	sh "$root/scripts/release-install.sh" "$version" "$out"
else
	echo "release: built $targets; once all four archives and the notices are in one folder, run scripts/release-install.sh $version <folder> to write install.sh and SHA256SUMS"
fi
