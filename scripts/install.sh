#!/bin/sh
set -eu

version='@VERSION@'
sha256_linux_amd64='@SHA256_LINUX_AMD64@'
sha256_linux_arm64='@SHA256_LINUX_ARM64@'
sha256_darwin_amd64='@SHA256_DARWIN_AMD64@'
sha256_darwin_arm64='@SHA256_DARWIN_ARM64@'

repo=bezmoradi/t2
latest="https://github.com/$repo/releases/latest/download/install.sh"
base="https://github.com/$repo/releases/download/v$version"
LC_ALL=C
export LC_ALL

tmp=
stage=

stop() {
	printf 'Install stopped: %s\n' "$1" >&2
	exit 1
}

warn() {
	printf 'Warning: %s\n' "$1" >&2
}

have() {
	command -v "$1" >/dev/null 2>&1
}

cleanup() {
	[ -z "$stage" ] || rm -rf "$stage"
	[ -z "$tmp" ] || rm -rf "$tmp"
}

sha256_of() {
	if have sha256sum; then
		sum=$(sha256sum "$1") || return 1
	else
		sum=$(shasum -a 256 "$1") || return 1
	fi
	printf '%s\n' "${sum%% *}"
}

check_sums() {
	if have sha256sum; then
		sha256sum -c SHA256SUMS >/dev/null 2>&1
	else
		shasum -a 256 -c SHA256SUMS >/dev/null 2>&1
	fi
}

check_linux_tools() {
	if ! have parecord || ! have pacat; then
		warn "parecord or pacat isn't installed, and t2 records and beeps with them. On Debian or Ubuntu: sudo apt install pulseaudio-utils"
	fi
	case ${XDG_SESSION_TYPE:-} in
	x11)
		have xclip || have xsel || warn "neither xclip nor xsel is installed, and t2 copies the text with one of them on X11. On Debian or Ubuntu: sudo apt install xclip"
		have xdotool || warn "xdotool isn't installed, and t2 pastes with it on X11. On Debian or Ubuntu: sudo apt install xdotool"
		;;
	wayland)
		have wl-copy || warn "wl-copy isn't installed, and t2 copies the text with it on Wayland. On Debian or Ubuntu: sudo apt install wl-clipboard"
		have wtype || warn "wtype isn't installed, and t2 types the text with it on labwc, Sway, Hyprland and other compositors with the virtual-keyboard protocol. On Debian or Ubuntu: sudo apt install wtype"
		;;
	*)
		warn "XDG_SESSION_TYPE isn't x11 or wayland, so the clipboard and paste tools weren't checked. Install the ones for your session from https://github.com/$repo#linux"
		;;
	esac
}

main() {
	exec </dev/null
	case $version$sha256_linux_amd64$sha256_linux_arm64$sha256_darwin_amd64$sha256_darwin_arm64 in
	*@*) stop "this is the template in the repository, not a release's script. Install with: curl -fsSL $latest | sh" ;;
	esac

	system=$(uname -s)
	machine=$(uname -m)
	case $system in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) os= ;;
	esac
	case $machine in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) arch= ;;
	esac
	if [ -z "$os" ] || [ -z "$arch" ]; then
		stop "this is $system on $machine, and T2's releases are built only for linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64."
	fi
	case $os-$arch in
	linux-amd64) want=$sha256_linux_amd64 ;;
	linux-arm64) want=$sha256_linux_arm64 ;;
	darwin-amd64) want=$sha256_darwin_amd64 ;;
	darwin-arm64) want=$sha256_darwin_arm64 ;;
	esac

	have curl || stop "curl isn't installed. Install curl and run this again."
	have tar || stop "tar isn't installed. Install tar and run this again."
	if ! have sha256sum && ! have shasum; then
		stop "neither sha256sum nor shasum is installed, so the download can't be checked. Install coreutils (or perl's shasum) and run this again."
	fi

	if [ -n "${T2_INSTALL_DIR:-}" ]; then
		dir=$T2_INSTALL_DIR
	elif [ -n "${HOME:-}" ]; then
		dir=$HOME/.local/bin
	else
		stop "HOME isn't set. Set T2_INSTALL_DIR to the folder to install into."
	fi
	case $dir in
	/*) ;;
	*) dir="$(pwd)/$dir" ;;
	esac

	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/t2-install.XXXXXX" 2>/dev/null) || stop "couldn't make a temporary folder."

	name="t2-$version-$os-$arch"
	archive="$tmp/$name.tar.gz"
	echo "Downloading $base/$name.tar.gz"
	curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$archive" "$base/$name.tar.gz" || stop "couldn't download $base/$name.tar.gz; nothing in $dir was changed."
	if ! got=$(sha256_of "$archive") || [ "$got" != "$want" ]; then
		stop "$name.tar.gz doesn't match the SHA-256 this script carries, so nothing in $dir was changed."
	fi
	mkdir "$tmp/x" || stop "couldn't make a folder in $tmp."
	tar -xzf "$archive" -C "$tmp/x" --no-same-owner || stop "couldn't unpack $name.tar.gz; nothing in $dir was changed."
	pkg="$tmp/x/$name"
	[ -f "$pkg/SHA256SUMS" ] || stop "$name.tar.gz holds no $name/SHA256SUMS; nothing in $dir was changed."
	if grep -Evx '[0-9a-f]{64}  [A-Za-z0-9._-]+' "$pkg/SHA256SUMS" >/dev/null; then
		stop "$name/SHA256SUMS has a line that isn't a SHA-256 and a file name; nothing in $dir was changed."
	fi
	grep -Eqx '[0-9a-f]{64}  t2' "$pkg/SHA256SUMS" || stop "$name/SHA256SUMS doesn't list t2; nothing in $dir was changed."
	(cd "$pkg" && check_sums) || stop "the files in $name.tar.gz don't match its SHA256SUMS; nothing in $dir was changed."
	if [ -L "$pkg/t2" ] || [ ! -f "$pkg/t2" ] || [ ! -x "$pkg/t2" ]; then
		stop "t2 in $name.tar.gz isn't a program; nothing in $dir was changed."
	fi
	got=$("$pkg/t2" --version 2>/dev/null) || stop "the downloaded t2 doesn't run from $tmp; if TMPDIR doesn't allow running programs, set it to a folder that does. Nothing in $dir was changed."
	[ "$got" = "T2 (Talk to Text) v$version" ] || stop "the downloaded t2 doesn't report version v$version; nothing in $dir was changed."

	mkdir -p "$dir" 2>/dev/null || stop "couldn't make the folder $dir."
	[ ! -d "$dir/t2" ] || stop "$dir/t2 is a folder or a link to one; move it away and run this again. Nothing in $dir was changed."
	stage=$(mktemp -d "$dir/.t2-install.XXXXXX" 2>/dev/null) || stop "couldn't write in $dir; nothing in it was changed."
	if ! cp "$pkg/t2" "$stage/t2" || ! chmod 0755 "$stage/t2"; then
		stop "couldn't copy t2 into $dir; nothing in it was changed."
	fi
	mv -f "$stage/t2" "$dir/t2" || stop "couldn't move t2 into place in $dir."
	rm -rf "$stage"
	stage=

	installed=$("$dir/t2" --version) || stop "$dir/t2 doesn't run."
	echo "Installed $installed for $os/$arch in $dir/t2"
	[ "$os" != linux ] || check_linux_tools
	case ":${PATH:-}:" in
	*":$dir:"*) ;;
	*) echo "$dir isn't on PATH; add it in your shell's profile: export PATH=\"$dir:\$PATH\"" ;;
	esac
	echo "Run t2 to start; the first run asks for your AssemblyAI API key."
	echo "To check where this release came from, with a signed-in gh: curl -fsSLO $base/$name.tar.gz && gh attestation verify $name.tar.gz -R $repo --signer-workflow $repo/.github/workflows/release.yml --source-ref refs/tags/v$version"
}

main "$@"
