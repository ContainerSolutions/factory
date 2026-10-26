#!/usr/bin/env bash
# The workspace base's provisioning (P1 §2, design/workstreams/agent/LLD/workspace-host.md and
# workspace-kata.md). It runs as admin (passwordless sudo) in one of two modes,
# FACTORY_BASE_MODE:
#
#   - vm (the default): factory-agent host init --driver tart embeds it and runs it with
#     `tart exec -i <vm> bash -s` inside a booted clone of ghcr.io/cirruslabs/ubuntu (arm64).
#     Its sha256 is in basevm.json, so any edit here rebuilds the base.
#   - image: provision/kata/Dockerfile runs it in an OCI build of the kata driver's image
#     (amd64), where systemd isn't running: units are enabled, never started; the sysctl file
#     is written for the guest to apply at boot; there is no cloud-init. Any edit here changes
#     the build context's sha256 (katabase.json), so the image is rebuilt too.
#
# It installs the same versions on amd64 and arm64 (dpkg --print-architecture): every
# per-architecture sha256 is a pair, NAME_amd64 and NAME_arm64, and set_arch picks this
# machine's into NAME. It is idempotent: a section whose pinned version is already there is
# skipped.
#
# The harness CLIs (claude-code, codex) are installed and never run here, not even with
# --version: their presence is checked by reading the files ([ -x ], claude's ELF magic, cmp,
# readlink). vm_test.go's TestProvisionScript enforces it.
#
# Bumping a pin: set the version and both sha256s together, from the source named next to it.
#
# The pin pairs are read only through set_arch's indirect expansion, which shellcheck can't see.
# shellcheck disable=SC2034
set -euo pipefail

# kind: https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0. Each sha256 is the
# release's kind-linux-<arch>.sha256sum.
KIND_VERSION=v0.33.0
KIND_SHA256_amd64=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
KIND_SHA256_arm64=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2

# kubectl: https://dl.k8s.io/release/v1.37.1/bin/linux/<arch>/kubectl. Each sha256 is that
# URL's kubectl.sha256.
KUBECTL_VERSION=v1.37.1
KUBECTL_SHA256_amd64=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
KUBECTL_SHA256_arm64=ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7

# node: the 24.x LTS release at least two weeks old when pinned (2026-10-22; 24.22.x was newer).
# Each sha256 is node-v24.21.0-linux-<x64|arm64>.tar.gz's line in
# https://nodejs.org/dist/v24.21.0/SHASUMS256.txt.
NODE_VERSION=v24.21.0
NODE_SHA256_amd64=6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff
NODE_SHA256_arm64=724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5

# go: the toolchain in the factory's go.mod. Each sha256 is go1.26.9.linux-<arch>.tar.gz's in
# https://go.dev/dl/?mode=json&include=all.
GO_VERSION=go1.26.9
GO_SHA256_amd64=42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d
GO_SHA256_arm64=4a97373d49fcacdcf3694fea368a500b00ee3e963974f3e7514132717632f052

# Playwright: the version web/package.json pins for @playwright/test (npm checks its integrity
# hashes). It is installed globally in npm's own global prefix (node's directory), where
# `npx --no playwright` and `npm root -g` find it: docs-gif's record_ui.sh runs the first and
# puts the second on NODE_PATH, so its flows' require('@playwright/test') resolves from a
# scratch directory. Its Chromium (and the ffmpeg build Playwright records video with) go to
# PLAYWRIGHT_BROWSERS, which the in-VM runtime exports as PLAYWRIGHT_BROWSERS_PATH
# (vmrun.PlaywrightBrowsersPath): a run's HOME is a scratch directory, so Playwright's default,
# ~/.cache/ms-playwright, would be empty. Playwright downloads the browsers from its CDN.
PLAYWRIGHT_VERSION=1.63.0
PLAYWRIGHT_BROWSERS=/opt/ms-playwright

# The GIF tools docs-gif records with (ccf-skills' ccf-docs/skills/docs-gif): Playwright's
# Chromium (above), vhs with ttyd, ffmpeg and gifski. ffmpeg, ttyd (1.7.4, vhs needs >= 1.7.2)
# and vhs's first font come from Ubuntu's archive (GIF_PACKAGES).
#
# vhs: https://github.com/charmbracelet/vhs/releases/tag/v0.12.1 (2026-09-24, the newest release
# at least two weeks old when pinned, 2026-10-23). Each sha256 is
# vhs_0.12.1_Linux_<x86_64|arm64>.tar.gz's line in the release's checksums.txt, which matches
# its digest in GitHub's releases API. vhs
# drives a Chromium it finds on PATH (go-rod's LookPath: `chromium` among others), else it
# downloads one: /usr/local/bin/chromium links Playwright's. The runtime sets VHS_NO_SANDBOX,
# since Ubuntu 24.04's AppArmor keeps Chromium's sandbox from its user namespaces (Playwright
# turns the sandbox off itself); the VM is the task's sandbox.
VHS_VERSION=v0.12.1
VHS_SHA256_amd64=eb33787a5b1cebf91fe0b18bd231e2d71d36c1ff4babd99ddac1a6ab411efd10
VHS_SHA256_arm64=0b27029809fcc2ca2c381cceb854cbe03479a0f229b963e9bc3a2f2c36ffd3e7

# gifski: no Ubuntu or Debian release packages it, and its releases ship x86-64 linux binaries
# only, so it is built from https://crates.io/crates/gifski/1.34.0 (2025-07-13, the newest
# release) on both architectures (one path), with the crate's own Cargo.lock (--locked: cargo checks every dependency's
# checksum). The sha256 is the crate's checksum in crates.io's API (crates/gifski/1.34.0).
GIFSKI_VERSION=1.34.0
GIFSKI_SHA256=c246c795a61d4a2476fb1c8ab70bedfaa825c734882adc40e117fc837df81190
# The Rust toolchain that builds it, in $TMP only: rustc, cargo and rust-std 1.99.0 (2026-10-01,
# the newest stable at least two weeks old when pinned). Each sha256 is the component's
# <name>-1.99.0-<x86_64|aarch64>-unknown-linux-gnu.tar.xz.sha256 on static.rust-lang.org, which
# matches its xz_hash in channel-rust-1.99.0.toml.
RUST_VERSION=1.99.0
RUSTC_SHA256_amd64=77171ba2a0345fdf2abc4fedda55d6de078dae7a68527c28be8c77dcc9604bd5
RUSTC_SHA256_arm64=89c0f1a3a44df63c95e5d5f2ba093d0a12c5f5f57f8635f8a7a0ce50cf86600e
CARGO_SHA256_amd64=d7674918d28093097614cd9728b6ca60db9ea3038f640f0bd1e9a4188c7568ce
CARGO_SHA256_arm64=113229fcc16cc1ba004923a4cc275fbf3281dafaaf8b97269f9c00189d497fd4
RUST_STD_SHA256_amd64=3e58dff2d0b72196b5ea4e90536e174d400de88564a52694686b81e091169933
RUST_STD_SHA256_arm64=1cf7e2ef58ed1cfa6f1adea18e155cbf254c1951f39e5f5e3376879f55fd64a4

# claude-code: the version the owner's smoke run accepted (2026-10-20,
# internal/agent/harness/claudecode/flags.go). npm checks the integrity hashes of the package
# and of its native dependency.
#
# What its postinstall (`node install.cjs`) does, read from the 2.1.304 tarball (npm pack, not
# run): it picks the platform's optional dependency, here @anthropic-ai/claude-code-linux-arm64
# or, on amd64, @anthropic-ai/claude-code-linux-x64 (glibc; neither has scripts), and hard-links (or else copies) that package's native `claude` over
# the wrapper's bin/claude.exe, a 500-byte shell stub that only prints "claude native binary
# not installed", then chmods it 0755. It runs no program on linux (it spawns sysctl only for
# an x64 node on darwin) and never runs claude. npm 11 skips install scripts it isn't told to
# allow, which leaves the stub as `claude`: the install allows this one package's script
# (--allow-scripts=@anthropic-ai/claude-code), and claude_code_installed checks the native
# binary is in place, without running it.
CLAUDE_CODE_VERSION=2.1.304
CLAUDE_CODE_DIR=/usr/local/lib/node_modules/@anthropic-ai/claude-code
# That native package's directory, which set_arch picks.
CLAUDE_CODE_NATIVE_DIR=""

# codex: https://github.com/openai/codex/releases/tag/rust-v0.163.0, the release P1 phase 0
# ran. The arm64 sha256 is codex-aarch64-unknown-linux-musl.tar.gz's, recorded in P1 phase 2b's
# spec; the amd64 one is codex-x86_64-unknown-linux-musl.tar.gz's digest in GitHub's releases
# API (where the arm64 one matches too).
CODEX_VERSION=rust-v0.163.0
CODEX_SHA256_amd64=beb339e8ecd9ba057a6bb2dc81ad8ca474812edfbf0fc5056ca818797648dde4
CODEX_SHA256_arm64=a7261e65f4a96d8f4ca65a069c6cf2dc2c33defc341e48078acefda9461fffe4
# codex's code-mode host, from the same release. Each sha256 is
# codex-code-mode-host-<x86_64|aarch64>-unknown-linux-musl.tar.gz's digest in GitHub's releases
# API.
# Code mode (feature code_mode_host, on by default) runs the model's tool calls through it when
# the model's tool mode is code mode, and without it those calls fail closed. codex looks for
# it next to its own executable (install-context's code_mode_host_program: current_exe's
# directory, canonicalized), so it goes beside /usr/local/bin/codex.
CODEX_CODE_MODE_HOST_SHA256_amd64=314263128ba6592e7e807c265deeaf928a692b72dcbfeb5e0b20c9423b703529
CODEX_CODE_MODE_HOST_SHA256_arm64=284356aefb197f00b76b912efdfdbae8ccf3ea1f89b976dde064b7ccb43214ef

# The gh extensions the worker's base tooling needs (worker.md, deploy/poc/README.md). They are
# installed without gh, the way `gh extension install --pin <tag>` installs a binary extension:
# gh extension install needs a gh login, which the base build doesn't have. Each pin is the
# newest release at least two weeks old when pinned (2026-10-22); each sha256 is its
# linux-<arch> asset's digest in GitHub's releases API (repos/<owner>/<repo>/releases/tags/<tag>).
# https://github.com/agynio/gh-pr-review/releases/tag/v1.6.2 (2025-12-08).
GH_PR_REVIEW_VERSION=v1.6.2
GH_PR_REVIEW_SHA256_amd64=2b7d4f2416f195e98010cb3e2b5684b285808e62864f89dea8497dbe691d7ed0
GH_PR_REVIEW_SHA256_arm64=fea079f78e7224200d23de847b6210134ccec1a4d2dcff96ca8ca4ce1060fdde
# https://github.com/github/gh-stack/releases/tag/v0.2.0 (2026-10-02; v0.2.1, 2026-10-09, was newer).
GH_STACK_VERSION=v0.2.0
GH_STACK_SHA256_amd64=577065033b49d4505730ea0ad43ab1b0374b0d9b1308e9765647a6aa8eba3cb7
GH_STACK_SHA256_arm64=d29684746f337a580667605b34877e5624792bcc390e61473a050bd4839557bf

# The apt signing keys of Docker's and GitHub CLI's repos, checked before apt trusts them. Each
# sha256 is the key file fetched on 2026-10-22; bump it when the vendor rotates the key.
# https://download.docker.com/linux/ubuntu/gpg: Docker Release (CE deb), fingerprint
# 9DC858229FC7DD38854AE2D88D81803C0EBFCD88 (https://docs.docker.com/engine/install/ubuntu/).
DOCKER_KEY_SHA256=1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570
# https://cli.github.com/packages/githubcli-archive-keyring.gpg: GitHub CLI, fingerprints
# 2C6106201985B60E6C7AC87323F3D4EA75716059 and 7F38BBB59D064DBCB3D84D725612B36462313325
# (https://github.com/cli/cli/blob/trunk/docs/install_linux.md).
GH_KEY_SHA256=6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b

APT_PACKAGES=(ca-certificates curl gnupg git make jq openssl unzip python3 python3-venv bubblewrap)
DOCKER_PACKAGES=(docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin)
# ffmpeg, ttyd and JetBrains Mono (vhs's default font) for docs-gif; gcc, libc6-dev and
# xz-utils for gifski's build (cc links it; the Rust tarballs are .tar.xz).
GIF_PACKAGES=(ffmpeg ttyd fonts-jetbrains-mono gcc libc6-dev xz-utils)

# The user the workspace runs as: the cirruslabs images' admin (the kata Dockerfile makes it).
WS_USER=admin

# The per-architecture sha256s set_arch picks: NAME is NAME_amd64 or NAME_arm64.
ARCH_PINS=(KIND_SHA256 KUBECTL_SHA256 NODE_SHA256 GO_SHA256 VHS_SHA256 RUSTC_SHA256 CARGO_SHA256 RUST_STD_SHA256 CODEX_SHA256 CODEX_CODE_MODE_HOST_SHA256 GH_PR_REVIEW_SHA256 GH_STACK_SHA256)

TMP=""
# Set by set_arch: dpkg's architecture (amd64, arm64) and the names release assets give it:
# node's (x64, arm64), vhs's (x86_64, arm64) and the target triples' (x86_64, aarch64).
ARCH=""
NODE_ARCH=""
VHS_ARCH=""
TRIPLE_ARCH=""

# set_arch picks this machine's architecture: its asset names and its half of each pin pair.
# Any architecture but amd64 and arm64 is an error.
set_arch() {
	ARCH="$(dpkg --print-architecture)"
	case "$ARCH" in
	amd64) NODE_ARCH=x64 VHS_ARCH=x86_64 TRIPLE_ARCH=x86_64 ;;
	arm64) NODE_ARCH=arm64 VHS_ARCH=arm64 TRIPLE_ARCH=aarch64 ;;
	*)
		echo "base.sh: unsupported architecture $ARCH (amd64 or arm64)" >&2
		exit 1
		;;
	esac
	local pin pair
	for pin in "${ARCH_PINS[@]}"; do
		pair="${pin}_$ARCH"
		printf -v "$pin" '%s' "${!pair}"
	done
	CLAUDE_CODE_NATIVE_DIR="$CLAUDE_CODE_DIR/node_modules/@anthropic-ai/claude-code-linux-$NODE_ARCH"
}

# set_mode checks FACTORY_BASE_MODE: vm (the default) or image.
set_mode() {
	FACTORY_BASE_MODE="${FACTORY_BASE_MODE:-vm}"
	case "$FACTORY_BASE_MODE" in
	vm | image) ;;
	*)
		echo "base.sh: unknown FACTORY_BASE_MODE $FACTORY_BASE_MODE (vm or image)" >&2
		exit 1
		;;
	esac
}

# image is true in the image mode: an OCI build, where systemd isn't running and there is no
# cloud-init. Every command that needs either runs only inside an `if ! image; then` block.
image() {
	[ "$FACTORY_BASE_MODE" = image ]
}

apt_get() {
	sudo DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 -y "$@"
}

# fetch URL SHA256 DEST downloads URL to DEST and checks its sha256.
fetch() {
	curl -fsSL --retry 3 --proto '=https' -o "$3" "$1"
	echo "$2  $3" | sha256sum -c --quiet -
}

# installed PATH SHA256 is true when PATH exists with that sha256.
installed() {
	[ -f "$1" ] && [ "$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ]
}

# apt_key URL SHA256 NAME installs the apt signing key at URL, with that sha256, as
# /etc/apt/keyrings/NAME. A key already there with the sha256 is left alone; any other is
# replaced, so a partial download is never kept.
apt_key() {
	installed "/etc/apt/keyrings/$3" "$2" && return
	fetch "$1" "$2" "$TMP/$3"
	sudo install -m 0644 "$TMP/$3" "/etc/apt/keyrings/$3"
}

# no_apt_timers stops Ubuntu's background apt runs, in this build and in every clone: they
# take dpkg's lock under a task's own apt-get and reach for the archive through the gateway.
# An image has nothing running to stop: the masks are enough.
no_apt_timers() {
	echo "== apt timers off"
	if ! image; then
		sudo systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
	fi
	sudo systemctl mask apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service
	if ! image; then
		sudo systemctl stop unattended-upgrades.service 2>/dev/null || true
	fi
	printf 'APT::Periodic::Enable "0";\nAPT::Periodic::Update-Package-Lists "0";\nAPT::Periodic::Unattended-Upgrade "0";\n' |
		sudo tee /etc/apt/apt.conf.d/99-factory-no-periodic >/dev/null
}

section_apt() {
	echo "== apt packages"
	apt_get update
	apt_get install --no-install-recommends "${APT_PACKAGES[@]}"
	sudo install -d -m 0755 /etc/apt/keyrings
}

section_docker() {
	echo "== docker"
	local codename
	codename="$(. /etc/os-release; echo "$VERSION_CODENAME")"
	apt_key https://download.docker.com/linux/ubuntu/gpg "$DOCKER_KEY_SHA256" docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" |
		sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
	apt_get update
	apt_get install "${DOCKER_PACKAGES[@]}"
	sudo usermod -aG docker "$WS_USER"
	if ! image; then
		sudo systemctl enable --now docker
	else
		sudo systemctl enable docker
	fi
}

section_kind() {
	echo "== kind $KIND_VERSION and kubectl $KUBECTL_VERSION"
	if ! installed /usr/local/bin/kind "$KIND_SHA256"; then
		fetch "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-$ARCH" "$KIND_SHA256" "$TMP/kind"
		sudo install -m 0755 "$TMP/kind" /usr/local/bin/kind
	fi
	if ! installed /usr/local/bin/kubectl "$KUBECTL_SHA256"; then
		fetch "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$ARCH/kubectl" "$KUBECTL_SHA256" "$TMP/kubectl"
		sudo install -m 0755 "$TMP/kubectl" /usr/local/bin/kubectl
	fi
	# kind's nodes need more inotify watches than Ubuntu's defaults. An image's guest applies
	# the file when it boots.
	printf 'fs.inotify.max_user_watches = 524288\nfs.inotify.max_user_instances = 512\n' |
		sudo tee /etc/sysctl.d/90-factory-kind.conf >/dev/null
	if ! image; then
		sudo sysctl --quiet --system
	fi
	# the warm base saves warm.kindImages here; the in-VM runtime loads them with kind (phase 3).
	sudo install -d -m 0755 /var/lib/factory /var/lib/factory/kind-images
}

section_gh() {
	echo "== gh and its extensions"
	apt_key https://cli.github.com/packages/githubcli-archive-keyring.gpg "$GH_KEY_SHA256" githubcli-archive-keyring.gpg
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" |
		sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null
	apt_get update
	apt_get install gh
	gh_extension agynio gh-pr-review "$GH_PR_REVIEW_VERSION" "$GH_PR_REVIEW_SHA256"
	gh_extension github gh-stack "$GH_STACK_VERSION" "$GH_STACK_SHA256"
}

# gh_extension OWNER REPO TAG SHA256 installs github.com/OWNER/REPO's linux-$ARCH release binary
# as WS_USER's pinned gh extension: <home>/.local/share/gh/extensions/REPO/REPO, executable, and
# the manifest.yml gh writes for a binary extension. It never runs gh: an extension whose binary
# has the sha256 and whose manifest matches is left alone.
gh_extension() {
	local owner=$1 repo=$2 tag=$3 sha=$4 home dir manifest
	home="$(getent passwd "$WS_USER" | cut -d: -f6)"
	dir="$home/.local/share/gh/extensions/$repo"
	manifest="owner: $owner
name: $repo
host: github.com
tag: $tag
ispinned: true
path: $dir/$repo"
	if installed "$dir/$repo" "$sha" && [ "$(sudo cat "$dir/manifest.yml" 2>/dev/null || true)" = "$manifest" ]; then
		return
	fi
	fetch "https://github.com/$owner/$repo/releases/download/$tag/linux-$ARCH" "$sha" "$TMP/$repo"
	printf '%s\n' "$manifest" >"$TMP/$repo.manifest.yml"
	# as WS_USER, so the dirs it makes (~/.local included) are theirs.
	sudo -u "$WS_USER" mkdir -p "$dir"
	sudo install -o "$WS_USER" -g "$(id -gn "$WS_USER")" -m 0755 "$TMP/$repo" "$dir/$repo"
	sudo install -o "$WS_USER" -g "$(id -gn "$WS_USER")" -m 0600 "$TMP/$repo.manifest.yml" "$dir/manifest.yml"
}

section_node() {
	echo "== node $NODE_VERSION"
	if [ "$(/usr/local/lib/node/bin/node --version 2>/dev/null || true)" != "$NODE_VERSION" ]; then
		fetch "https://nodejs.org/dist/$NODE_VERSION/node-$NODE_VERSION-linux-$NODE_ARCH.tar.gz" "$NODE_SHA256" "$TMP/node.tar.gz"
		sudo rm -rf /usr/local/lib/node
		sudo install -d -m 0755 /usr/local/lib/node
		sudo tar -xzf "$TMP/node.tar.gz" -C /usr/local/lib/node --strip-components=1 --no-same-owner
	fi
	local b
	for b in node npm npx corepack; do
		sudo ln -sfn "/usr/local/lib/node/bin/$b" "/usr/local/bin/$b"
	done
	# yarn and pnpm shims, next to node.
	sudo corepack enable --install-directory /usr/local/bin
}

section_go() {
	echo "== go $GO_VERSION"
	if [ "$(head -n 1 /usr/local/go/VERSION 2>/dev/null || true)" != "$GO_VERSION" ]; then
		fetch "https://go.dev/dl/$GO_VERSION.linux-$ARCH.tar.gz" "$GO_SHA256" "$TMP/go.tar.gz"
		sudo rm -rf /usr/local/go
		sudo tar -xzf "$TMP/go.tar.gz" -C /usr/local --no-same-owner
	fi
	sudo ln -sfn /usr/local/go/bin/go /usr/local/bin/go
	sudo ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

section_playwright() {
	echo "== playwright $PLAYWRIGHT_VERSION, its system dependencies and its Chromium"
	local prefix root chrome
	prefix="$(npm prefix -g)"
	root="$(npm root -g)"
	if [ "$(version "$root/@playwright/test")" != "$PLAYWRIGHT_VERSION" ] || [ ! -x "$prefix/bin/playwright" ]; then
		sudo npm install -g --prefix "$prefix" --no-fund --no-audit "@playwright/test@$PLAYWRIGHT_VERSION"
	fi
	sudo env DEBIAN_FRONTEND=noninteractive "$prefix/bin/playwright" install-deps chromium
	# an installed browser is skipped.
	sudo install -d -m 0755 "$PLAYWRIGHT_BROWSERS"
	sudo env PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS" "$prefix/bin/playwright" install chromium
	sudo chmod -R a+rX "$PLAYWRIGHT_BROWSERS"
	# vhs's Chromium: the one Playwright launches.
	chrome="$(PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS" NODE_PATH="$root" node -e 'process.stdout.write(require("@playwright/test").chromium.executablePath())')"
	[ -x "$chrome" ] || { echo "playwright's chromium is missing: $chrome" >&2; exit 1; }
	sudo ln -sfn "$chrome" /usr/local/bin/chromium
}

# rust_component NAME SHA256 installs the Rust toolchain component NAME at RUST_VERSION in
# $TMP/rust.
rust_component() {
	local dir="$1-$RUST_VERSION-$TRIPLE_ARCH-unknown-linux-gnu"
	fetch "https://static.rust-lang.org/dist/$dir.tar.xz" "$2" "$TMP/$dir.tar.xz"
	tar -xJf "$TMP/$dir.tar.xz" -C "$TMP" --no-same-owner
	"$TMP/$dir/install.sh" --prefix="$TMP/rust" --disable-ldconfig >/dev/null
}

section_gif_tools() {
	echo "== GIF tools: ffmpeg, ttyd, vhs $VHS_VERSION, gifski $GIFSKI_VERSION"
	apt_get install --no-install-recommends "${GIF_PACKAGES[@]}"

	if [ "$(cat /var/lib/factory/vhs.tar.sha256 2>/dev/null || true)" != "$VHS_SHA256" ] || [ ! -x /usr/local/bin/vhs ]; then
		fetch "https://github.com/charmbracelet/vhs/releases/download/$VHS_VERSION/vhs_${VHS_VERSION#v}_Linux_$VHS_ARCH.tar.gz" "$VHS_SHA256" "$TMP/vhs.tar.gz"
		mkdir "$TMP/vhs"
		tar -xzf "$TMP/vhs.tar.gz" -C "$TMP/vhs" --no-same-owner
		sudo install -m 0755 "$TMP/vhs/vhs_${VHS_VERSION#v}_Linux_$VHS_ARCH/vhs" /usr/local/bin/vhs
		echo "$VHS_SHA256" | sudo tee /var/lib/factory/vhs.tar.sha256 >/dev/null
	fi

	if [ "$(cat /var/lib/factory/gifski.crate.sha256 2>/dev/null || true)" != "$GIFSKI_SHA256" ] || [ ! -x /usr/local/bin/gifski ]; then
		rust_component rustc "$RUSTC_SHA256"
		rust_component cargo "$CARGO_SHA256"
		rust_component rust-std "$RUST_STD_SHA256"
		fetch "https://static.crates.io/crates/gifski/gifski-$GIFSKI_VERSION.crate" "$GIFSKI_SHA256" "$TMP/gifski.crate"
		tar -xzf "$TMP/gifski.crate" -C "$TMP" --no-same-owner
		CARGO_HOME="$TMP/cargo" PATH="$TMP/rust/bin:$PATH" cargo install --locked --path "$TMP/gifski-$GIFSKI_VERSION" --root "$TMP/gifski"
		sudo install -m 0755 "$TMP/gifski/bin/gifski" /usr/local/bin/gifski
		echo "$GIFSKI_SHA256" | sudo tee /var/lib/factory/gifski.crate.sha256 >/dev/null
	fi
	[ -x /usr/local/bin/vhs ] && [ -x /usr/local/bin/gifski ] || { echo "vhs or gifski is missing from /usr/local/bin" >&2; exit 1; }
}

# version DIR is DIR/package.json's version, or "".
version() {
	jq -r .version "$1/package.json" 2>/dev/null || true
}

# elf PATH is true when PATH is an ELF binary (its first bytes, read without running it).
elf() {
	[ "$(head -c 4 "$1" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ]
}

# claude_code_installed is true when the pinned claude-code is installed with its native
# binary in place: /usr/local/bin/claude resolves to the wrapper's bin/claude.exe, which is the
# native package's ELF binary (not the stub), executable. It reads the files, never runs them.
claude_code_installed() {
	[ "$(version "$CLAUDE_CODE_DIR")" = "$CLAUDE_CODE_VERSION" ] &&
		[ "$(version "$CLAUDE_CODE_NATIVE_DIR")" = "$CLAUDE_CODE_VERSION" ] &&
		elf "$CLAUDE_CODE_NATIVE_DIR/claude" &&
		[ -x "$CLAUDE_CODE_DIR/bin/claude.exe" ] &&
		cmp -s "$CLAUDE_CODE_NATIVE_DIR/claude" "$CLAUDE_CODE_DIR/bin/claude.exe" &&
		[ "$(readlink -f /usr/local/bin/claude)" = "$CLAUDE_CODE_DIR/bin/claude.exe" ]
}

section_harnesses() {
	echo "== harness CLIs (installed, never run)"
	if ! claude_code_installed; then
		sudo npm install -g --prefix /usr/local --no-fund --no-audit --allow-scripts=@anthropic-ai/claude-code "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
	fi
	claude_code_installed || { echo "claude-code's native binary isn't in place: $CLAUDE_CODE_DIR/bin/claude.exe" >&2; exit 1; }

	if [ "$(cat /var/lib/factory/codex.tar.sha256 2>/dev/null || true)" != "$CODEX_SHA256" ] || [ ! -x /usr/local/bin/codex ]; then
		fetch "https://github.com/openai/codex/releases/download/$CODEX_VERSION/codex-$TRIPLE_ARCH-unknown-linux-musl.tar.gz" "$CODEX_SHA256" "$TMP/codex.tar.gz"
		mkdir "$TMP/codex"
		tar -xzf "$TMP/codex.tar.gz" -C "$TMP/codex" --no-same-owner
		sudo install -m 0755 "$TMP/codex/codex-$TRIPLE_ARCH-unknown-linux-musl" /usr/local/bin/codex
		echo "$CODEX_SHA256" | sudo tee /var/lib/factory/codex.tar.sha256 >/dev/null
	fi
	[ -x /usr/local/bin/codex ] || { echo "codex is missing: /usr/local/bin/codex" >&2; exit 1; }

	if [ "$(cat /var/lib/factory/codex-code-mode-host.tar.sha256 2>/dev/null || true)" != "$CODEX_CODE_MODE_HOST_SHA256" ] || [ ! -x /usr/local/bin/codex-code-mode-host ]; then
		fetch "https://github.com/openai/codex/releases/download/$CODEX_VERSION/codex-code-mode-host-$TRIPLE_ARCH-unknown-linux-musl.tar.gz" "$CODEX_CODE_MODE_HOST_SHA256" "$TMP/codex-code-mode-host.tar.gz"
		mkdir "$TMP/codex-code-mode-host"
		tar -xzf "$TMP/codex-code-mode-host.tar.gz" -C "$TMP/codex-code-mode-host" --no-same-owner
		sudo install -m 0755 "$TMP/codex-code-mode-host/codex-code-mode-host-$TRIPLE_ARCH-unknown-linux-musl" /usr/local/bin/codex-code-mode-host
		echo "$CODEX_CODE_MODE_HOST_SHA256" | sudo tee /var/lib/factory/codex-code-mode-host.tar.sha256 >/dev/null
	fi
	[ -x /usr/local/bin/codex-code-mode-host ] || { echo "codex's code-mode host is missing: /usr/local/bin/codex-code-mode-host" >&2; exit 1; }
}

section_cleanup() {
	echo "== cleanup"
	sudo npm cache clean --force --loglevel=error
	apt_get clean
	sudo rm -rf /var/lib/apt/lists/*
	# clones get fresh identities: cloud-init runs again and systemd makes a new machine-id.
	# An image has no cloud-init; each workspace's systemd makes its own machine-id.
	if ! image; then
		sudo cloud-init clean --logs
	fi
	sudo truncate -s 0 /etc/machine-id
}

main() {
	set_mode
	set_arch
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	# the first boot's cloud-init may still hold apt's lock; the apt timers are stopped first.
	no_apt_timers
	if ! image; then
		if command -v cloud-init >/dev/null; then
			sudo cloud-init status --wait >/dev/null || true
		fi
	fi
	section_apt
	section_docker
	section_kind
	section_gh
	section_node
	section_go
	section_playwright
	section_gif_tools
	section_harnesses
	section_cleanup
	echo "== done"
}

# bash reads this script from stdin: main runs only once all of it is read, and with stdin
# from /dev/null, so no command it runs can swallow the rest of the script.
main "$@" </dev/null
