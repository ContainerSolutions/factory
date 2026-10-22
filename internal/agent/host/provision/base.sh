#!/usr/bin/env bash
# The workspace base VM's provisioning (P1 §2, design/workstreams/agent/LLD/workspace-host.md).
# factory-agent host init embeds it and runs it with `tart exec -i <vm> bash -s` inside a clone
# of ghcr.io/cirruslabs/ubuntu, as admin (passwordless sudo). It is idempotent: a section
# whose pinned version is already there is skipped. Its sha256 is in basevm.json, so any edit
# here rebuilds the base.
#
# The harness CLIs (claude-code, codex) are installed and never run here, not even with
# --version: their presence is checked with [ -x ]. host_test.go's TestProvisionScript
# enforces it.
#
# Bumping a pin: set the version and the sha256 together, from the source named next to it.
set -euo pipefail

# kind: https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0. The sha256 is the release's
# kind-linux-arm64.sha256sum.
KIND_VERSION=v0.33.0
KIND_SHA256=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2

# kubectl: https://dl.k8s.io/release/v1.37.1/bin/linux/arm64/kubectl. The sha256 is that URL's
# kubectl.sha256.
KUBECTL_VERSION=v1.37.1
KUBECTL_SHA256=ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7

# node: the 24.x LTS release at least two weeks old when pinned (2026-10-22; 24.22.x was newer).
# The sha256 is node-v24.21.0-linux-arm64.tar.gz's line in
# https://nodejs.org/dist/v24.21.0/SHASUMS256.txt.
NODE_VERSION=v24.21.0
NODE_SHA256=724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5

# go: the toolchain in the factory's go.mod. The sha256 is go1.26.9.linux-arm64.tar.gz's in
# https://go.dev/dl/?mode=json&include=all.
GO_VERSION=go1.26.9
GO_SHA256=4a97373d49fcacdcf3694fea368a500b00ee3e963974f3e7514132717632f052

# Playwright: the version web/package.json pins for @playwright/test. Only its system
# dependencies are installed (the browsers come with a task's own Playwright).
PLAYWRIGHT_VERSION=1.63.0

# claude-code: the version the owner's smoke run accepted (2026-10-20,
# internal/agent/harness/claudecode/flags.go). npm checks the package's integrity hash.
CLAUDE_CODE_VERSION=2.1.304

# codex: https://github.com/openai/codex/releases/tag/rust-v0.163.0, the release P1 phase 0
# ran. The sha256 is codex-aarch64-unknown-linux-musl.tar.gz's, recorded in P1 phase 2b's spec.
CODEX_VERSION=rust-v0.163.0
CODEX_SHA256=a7261e65f4a96d8f4ca65a069c6cf2dc2c33defc341e48078acefda9461fffe4

# The gh extensions the worker's base tooling needs (worker.md, deploy/poc/README.md). They are
# installed without gh, the way `gh extension install --pin <tag>` installs a binary extension:
# gh extension install needs a gh login, which the base build doesn't have. Each pin is the
# newest release at least two weeks old when pinned (2026-10-22); the sha256 is its linux-arm64
# asset's digest in GitHub's releases API (repos/<owner>/<repo>/releases/tags/<tag>).
# https://github.com/agynio/gh-pr-review/releases/tag/v1.6.2 (2025-12-08).
GH_PR_REVIEW_VERSION=v1.6.2
GH_PR_REVIEW_SHA256=fea079f78e7224200d23de847b6210134ccec1a4d2dcff96ca8ca4ce1060fdde
# https://github.com/github/gh-stack/releases/tag/v0.2.0 (2026-10-02; v0.2.1, 2026-10-09, was newer).
GH_STACK_VERSION=v0.2.0
GH_STACK_SHA256=d29684746f337a580667605b34877e5624792bcc390e61473a050bd4839557bf

APT_PACKAGES=(ca-certificates curl gnupg git make jq openssl unzip python3 python3-venv bubblewrap)
DOCKER_PACKAGES=(docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin)

# The user the workspace runs as: the cirruslabs images' admin.
WS_USER=admin

TMP=""

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
	if [ ! -f /etc/apt/keyrings/docker.asc ]; then
		sudo curl -fsSL --proto '=https' -o /etc/apt/keyrings/docker.asc https://download.docker.com/linux/ubuntu/gpg
		sudo chmod 0644 /etc/apt/keyrings/docker.asc
	fi
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" |
		sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
	apt_get update
	apt_get install "${DOCKER_PACKAGES[@]}"
	sudo usermod -aG docker "$WS_USER"
	sudo systemctl enable --now docker
}

section_kind() {
	echo "== kind $KIND_VERSION and kubectl $KUBECTL_VERSION"
	if ! installed /usr/local/bin/kind "$KIND_SHA256"; then
		fetch "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-arm64" "$KIND_SHA256" "$TMP/kind"
		sudo install -m 0755 "$TMP/kind" /usr/local/bin/kind
	fi
	if ! installed /usr/local/bin/kubectl "$KUBECTL_SHA256"; then
		fetch "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/arm64/kubectl" "$KUBECTL_SHA256" "$TMP/kubectl"
		sudo install -m 0755 "$TMP/kubectl" /usr/local/bin/kubectl
	fi
	# kind's nodes need more inotify watches than Ubuntu's defaults.
	printf 'fs.inotify.max_user_watches = 524288\nfs.inotify.max_user_instances = 512\n' |
		sudo tee /etc/sysctl.d/90-factory-kind.conf >/dev/null
	sudo sysctl --quiet --system
	# the warm base saves warm.kindImages here; the in-VM runtime loads them with kind (phase 3).
	sudo install -d -m 0755 /var/lib/factory /var/lib/factory/kind-images
}

section_gh() {
	echo "== gh and its extensions"
	if [ ! -f /etc/apt/keyrings/githubcli-archive-keyring.gpg ]; then
		sudo curl -fsSL --proto '=https' -o /etc/apt/keyrings/githubcli-archive-keyring.gpg https://cli.github.com/packages/githubcli-archive-keyring.gpg
		sudo chmod 0644 /etc/apt/keyrings/githubcli-archive-keyring.gpg
	fi
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" |
		sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null
	apt_get update
	apt_get install gh
	gh_extension agynio gh-pr-review "$GH_PR_REVIEW_VERSION" "$GH_PR_REVIEW_SHA256"
	gh_extension github gh-stack "$GH_STACK_VERSION" "$GH_STACK_SHA256"
}

# gh_extension OWNER REPO TAG SHA256 installs github.com/OWNER/REPO's linux-arm64 release binary
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
	fetch "https://github.com/$owner/$repo/releases/download/$tag/linux-arm64" "$sha" "$TMP/$repo"
	printf '%s\n' "$manifest" >"$TMP/$repo.manifest.yml"
	# as WS_USER, so the dirs it makes (~/.local included) are theirs.
	sudo -u "$WS_USER" mkdir -p "$dir"
	sudo install -o "$WS_USER" -g "$(id -gn "$WS_USER")" -m 0755 "$TMP/$repo" "$dir/$repo"
	sudo install -o "$WS_USER" -g "$(id -gn "$WS_USER")" -m 0600 "$TMP/$repo.manifest.yml" "$dir/manifest.yml"
}

section_node() {
	echo "== node $NODE_VERSION"
	if [ "$(/usr/local/lib/node/bin/node --version 2>/dev/null || true)" != "$NODE_VERSION" ]; then
		fetch "https://nodejs.org/dist/$NODE_VERSION/node-$NODE_VERSION-linux-arm64.tar.gz" "$NODE_SHA256" "$TMP/node.tar.gz"
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
		fetch "https://go.dev/dl/$GO_VERSION.linux-arm64.tar.gz" "$GO_SHA256" "$TMP/go.tar.gz"
		sudo rm -rf /usr/local/go
		sudo tar -xzf "$TMP/go.tar.gz" -C /usr/local --no-same-owner
	fi
	sudo ln -sfn /usr/local/go/bin/go /usr/local/bin/go
	sudo ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

section_playwright() {
	echo "== playwright $PLAYWRIGHT_VERSION system dependencies"
	sudo env DEBIAN_FRONTEND=noninteractive npx -y "playwright@$PLAYWRIGHT_VERSION" install-deps chromium
}

section_harnesses() {
	echo "== harness CLIs (installed, never run)"
	local have
	have="$(jq -r .version /usr/local/lib/node_modules/@anthropic-ai/claude-code/package.json 2>/dev/null || true)"
	if [ "$have" != "$CLAUDE_CODE_VERSION" ]; then
		sudo npm install -g --prefix /usr/local --no-fund --no-audit "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
	fi
	[ -x /usr/local/bin/claude ] || { echo "claude-code's bin is missing: /usr/local/bin/claude" >&2; exit 1; }

	if [ "$(cat /var/lib/factory/codex.tar.sha256 2>/dev/null || true)" != "$CODEX_SHA256" ] || [ ! -x /usr/local/bin/codex ]; then
		fetch "https://github.com/openai/codex/releases/download/$CODEX_VERSION/codex-aarch64-unknown-linux-musl.tar.gz" "$CODEX_SHA256" "$TMP/codex.tar.gz"
		mkdir "$TMP/codex"
		tar -xzf "$TMP/codex.tar.gz" -C "$TMP/codex" --no-same-owner
		sudo install -m 0755 "$TMP/codex/codex-aarch64-unknown-linux-musl" /usr/local/bin/codex
		echo "$CODEX_SHA256" | sudo tee /var/lib/factory/codex.tar.sha256 >/dev/null
	fi
	[ -x /usr/local/bin/codex ] || { echo "codex is missing: /usr/local/bin/codex" >&2; exit 1; }
}

section_cleanup() {
	echo "== cleanup"
	sudo npm cache clean --force --loglevel=error
	apt_get clean
	sudo rm -rf /var/lib/apt/lists/*
	# clones get fresh identities: cloud-init runs again and systemd makes a new machine-id.
	sudo cloud-init clean --logs
	sudo truncate -s 0 /etc/machine-id
}

main() {
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	# the first boot's cloud-init and apt timers may still hold apt's lock.
	if command -v cloud-init >/dev/null; then
		sudo cloud-init status --wait >/dev/null || true
	fi
	section_apt
	section_docker
	section_kind
	section_gh
	section_node
	section_go
	section_playwright
	section_harnesses
	section_cleanup
	echo "== done"
}

# bash reads this script from stdin: main runs only once all of it is read, and with stdin
# from /dev/null, so no command it runs can swallow the rest of the script.
main "$@" </dev/null
