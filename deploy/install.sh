#!/usr/bin/env bash
# Sets up this machine as a factory agent host from a GitHub release of ContainerSolutions/factory
# (deploy/poc/README.md §Agent onboarding): an Apple-silicon Mac (tart) or a Linux x86-64 host
# (kata). Every release carries it, so
#
#   curl -fsSL https://github.com/ContainerSolutions/factory/releases/latest/download/install.sh | bash
#
# installs the latest release (flags: `| bash -s -- --dry-run`). It downloads factory-agent (and,
# on a Mac, factory-agent-linux-arm64 for the VMs) and checks them against SHA256SUMS; installs
# them to ~/.local/bin; writes ~/.config/factory-agent/agent.yaml and the enrollment token unless
# they exist; runs factory-agent host init; offers the model logins; and installs supervise as a
# launchd user agent (Mac) or a systemd user service (Linux). Run it as the owner's own user, not
# root. Prompts read /dev/tty: under curl | bash, stdin is this script.
#
# Flags:
#   --dry-run     print every step and command; change nothing, download nothing
#   --yes         no prompts: values from the environment, failing when one is missing; the model
#                 logins are left for later (it prints the commands)
#   --no-service  don't install supervise as a service
# Environment:
#   FACTORY_VERSION           release tag to install (default: the latest release)
#   FACTORY_PREFIX            where the binaries go (default ~/.local/bin)
#   FACTORY_DISPATCHER        the dispatcher's URL, for a new agent.yaml
#   FACTORY_POOL              the Pool to join, for a new agent.yaml (default local)
#   FACTORY_HOST              the agent's host name (default: this machine's short host name)
#   FACTORY_ENROLLMENT_TOKEN  the enrollment token, when its file is missing
set -euo pipefail

repo_url="https://github.com/ContainerSolutions/factory"
label="com.containersolutions.factory-agent"

dry_run=0
yes=0
service=1

die() {
	echo "install.sh: $*" >&2
	exit 1
}
say() { printf '==> %s\n' "$*"; }

usage() {
	if [ -f "${BASH_SOURCE[0]:-}" ]; then
		sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -e '/^set -euo/d' -e 's/^# \{0,1\}//'
	else
		echo "usage: install.sh [--dry-run] [--yes] [--no-service]  (deploy/install.sh's header says more)"
	fi
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) dry_run=1 ;;
	--yes) yes=1 ;;
	--no-service) service=0 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown flag $1 (--dry-run, --yes, --no-service)" ;;
	esac
	shift
done

[ "$(id -u)" != 0 ] || die "run this as the agent owner's own user, not root (host init asks for sudo itself)"
[ -n "${HOME:-}" ] || die "HOME is not set"

# run prints a command and, unless --dry-run, runs it.
run() {
	printf '  $'
	printf ' %q' "$@"
	printf '\n'
	[ "$dry_run" = 1 ] || "$@"
}

# ask VAR ENV PROMPT [silent]: reads a value from the terminal into VAR. With --yes, or with no
# terminal, it fails instead, naming the environment variable ENV that would have set it.
ask() {
	local var="$1" env="$2" prompt="$3" silent="${4:-}" value=""
	[ "$yes" = 0 ] || die "--yes: set $env"
	[ -r /dev/tty ] || die "no terminal to ask on: set $env"
	if [ -n "$silent" ]; then
		read -rs -p "$prompt" value </dev/tty
		echo >/dev/tty
	else
		read -r -p "$prompt" value </dev/tty
	fi
	printf -v "$var" '%s' "$value"
}

# value VAR ENV PROMPT [silent]: VAR is the environment variable ENV's value when set; else, in a
# dry run without --yes, the placeholder <asked> (nothing is asked); else the terminal's answer.
value() {
	local var="$1" env="$2"
	if [ -n "${!env:-}" ]; then
		printf -v "$var" '%s' "${!env}"
	elif [ "$dry_run" = 1 ] && [ "$yes" = 0 ]; then
		printf -v "$var" '%s' '<asked>'
	else
		ask "$var" "$env" "$3" "${4:-}"
	fi
}

# confirm PROMPT: y/N on the terminal; no with --yes or --dry-run.
confirm() {
	local answer=""
	[ "$yes" = 0 ] && [ "$dry_run" = 0 ] && [ -r /dev/tty ] || return 1
	read -r -p "$1 [y/N] " answer </dev/tty
	case "$answer" in y | Y | yes | YES) return 0 ;; *) return 1 ;; esac
}

# 1. The platform.
os="$(uname -s)"
arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
esac
case "$os/$arch" in
Darwin/arm64)
	goos=darwin driver=tart
	state_dir="$HOME/Library/Application Support/factory-agent"
	;;
Linux/amd64)
	goos=linux driver=kata
	case "${XDG_STATE_HOME:-}" in
	/*) state_dir="$XDG_STATE_HOME/factory-agent" ;;
	*) state_dir="$HOME/.local/state/factory-agent" ;;
	esac
	;;
*) die "unsupported platform $os/$arch: an agent host is an Apple-silicon Mac (tart) or a Linux x86-64 machine (kata)" ;;
esac
say "platform $goos/$arch: the $driver driver"
[ "$dry_run" = 0 ] || say "dry run: nothing is downloaded or changed"

# 2. The release, the downloads and their checksums.
version="${FACTORY_VERSION:-}"
if [ -n "$version" ]; then
	[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "FACTORY_VERSION $version is not a vX.Y.Z tag"
	base_url="$repo_url/releases/download/$version"
else
	version=latest
	base_url="$repo_url/releases/latest/download"
fi
assets=("factory-agent-$goos-$arch")
[ "$driver" != tart ] || assets+=(factory-agent-linux-arm64)
say "release $version: ${assets[*]} and SHA256SUMS from $base_url"

if [ "$goos" = linux ]; then
	sumcmd=(sha256sum -c -)
else
	sumcmd=(shasum -a 256 -c -)
fi

tmp=""
if [ "$dry_run" = 0 ]; then
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
else
	tmp='<a temp dir>'
fi
for f in SHA256SUMS "${assets[@]}"; do
	run curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$f" "$base_url/$f"
done
say "checking ${assets[*]} against SHA256SUMS (${sumcmd[*]})"
if [ "$dry_run" = 0 ]; then
	for f in "${assets[@]}"; do
		line="$(grep -E "^[0-9a-f]{64}  $f\$" "$tmp/SHA256SUMS")" || die "SHA256SUMS has no entry for $f"
		(cd "$tmp" && printf '%s\n' "$line" | "${sumcmd[@]}" >/dev/null) || die "$f does not match its SHA256SUMS entry: not installing it"
	done
fi

# 3. Install.
prefix="${FACTORY_PREFIX:-$HOME/.local/bin}"
agent="$prefix/factory-agent"
run mkdir -p "$prefix"
run install -m 0755 "$tmp/factory-agent-$goos-$arch" "$agent"
agent_linux=""
if [ "$driver" = tart ]; then
	agent_linux="$prefix/factory-agent-linux-arm64"
	run install -m 0755 "$tmp/factory-agent-linux-arm64" "$agent_linux"
fi
case ":$PATH:" in
*":$prefix:"*) ;;
*) say "$prefix is not on PATH: add it (the commands below use full paths)" ;;
esac

# 4. The config.
config_dir="$HOME/.config/factory-agent"
config="$config_dir/agent.yaml"
token_file="$config_dir/enrollment-token"
host="${FACTORY_HOST:-}"
if [ -z "$host" ]; then
	host="$(uname -n)"
	host="${host%%.*}"
	host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')"
	host="${host#"${host%%[!-]*}"}"
	host="${host:-agent}"
fi
[[ "$host" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "FACTORY_HOST $host: want lower-case letters, digits and '-'"
run mkdir -p -m 0700 "$config_dir"
if [ -e "$config" ]; then
	say "$config exists: leaving it as it is. Check its dispatcher, pool, host ($host),"
	say "enrollmentTokenFile ($token_file) and harnesses against deploy/poc/agent.yaml.example."
else
	dispatcher=""
	value dispatcher FACTORY_DISPATCHER "The dispatcher's URL (https://…): "
	[ "$dispatcher" = '<asked>' ] || [[ "$dispatcher" =~ ^https?://[A-Za-z0-9.:/_-]+$ ]] || die "dispatcher $dispatcher: want an http(s) URL"
	pool="${FACTORY_POOL:-local}"
	[[ "$pool" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "FACTORY_POOL $pool: want lower-case letters, digits and '-'"
	# The harness and lending blocks are deploy/poc/agent.yaml.example's (installsh_test.go checks).
	yaml="# factory-agent's config, written by install.sh. Every key: deploy/poc/agent.yaml.example.
dispatcher: $dispatcher
pool: $pool
host: $host
enrollmentTokenFile: ~/.config/factory-agent/enrollment-token
# A harness is offered when its login is stored (factory-agent login). The codex model is a
# placeholder: put a gpt-5* or gpt-6* model your codex sign-in offers (see /model in codex).
harnesses:
  codex:
    models: [gpt-6.1-sol]
    efforts: [low, medium, high]
    default: gpt-6.1-sol
  claude-code:
    models: [claude-sonnet-5-5, claude-opus-5-5]
    efforts: [low, medium, high, max]
    default: claude-sonnet-5-5
lending:
  unlimited: true"
	say "writing $config (0600):"
	printf -- '--- agent.yaml ---\n%s\n--- end agent.yaml ---\n' "$yaml"
	if [ "$dry_run" = 0 ]; then
		(umask 077 && printf '%s\n' "$yaml" >"$config")
	fi
fi

# 5. The enrollment token.
if [ -e "$token_file" ]; then
	say "$token_file exists: using it"
else
	say "the enrollment token: an admin of the dispatcher mints it for this host with"
	say "  factory agent enroll --pool ${FACTORY_POOL:-local} --owner <your GitHub login> --name $host --server <the dispatcher's URL>"
	token=""
	value token FACTORY_ENROLLMENT_TOKEN "Paste the enrollment token (not echoed): " silent
	[ -n "$token" ] || die "an empty enrollment token"
	case "$token" in *$'\n'*) die "the enrollment token must be a single line" ;; esac
	say "writing $token_file (0600)"
	if [ "$dry_run" = 0 ]; then
		(umask 077 && printf '%s\n' "$token" >"$token_file")
	fi
	unset token
fi

# 6. The host: host init asks for sudo itself.
init=("$agent" host init --driver "$driver")
[ "$driver" != tart ] || init+=(--agent-linux "$agent_linux")
[ "$yes" = 0 ] || init+=(--yes)
say "host init (it asks for sudo once; the VM images take a while)"
if [ "$dry_run" = 1 ]; then
	run "${init[@]}"
elif [ -r /dev/tty ]; then
	run "${init[@]}" </dev/tty
else
	run "${init[@]}" </dev/null
fi

# 7. The model logins, which only the gateway holds.
say "the model logins (each skippable; later: $agent login claude|codex)"
if confirm "Store a claude login now (paste the token claude setup-token printed)?"; then
	claude_token=""
	ask claude_token FACTORY_CLAUDE_TOKEN "Paste the claude setup-token token (not echoed): " silent
	printf '%s\n' "$claude_token" | "$agent" login claude
	unset claude_token
else
	say "  claude: run claude setup-token on its own, copy the token, then: $agent login claude"
fi
if confirm "Import the codex login (after codex login on this machine)?"; then
	"$agent" login codex
else
	say "  codex: codex login (with ChatGPT), then: $agent login codex"
fi

# 8. supervise as a service, running as this user (supervise.md: the workspaces are the user's).
log="$state_dir/supervise.log"
if [ "$service" = 0 ]; then
	say "--no-service: run supervise yourself: $agent supervise"
elif [ "$driver" = tart ]; then
	plist="$HOME/Library/LaunchAgents/$label.plist"
	say "installing supervise as the launchd user agent $label ($plist), logging to $log"
	content="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$agent</string><string>supervise</string></array>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>$prefix:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>60</integer>
  <key>StandardOutPath</key><string>$log</string>
  <key>StandardErrorPath</key><string>$log</string>
</dict>
</plist>"
	run mkdir -p "$HOME/Library/LaunchAgents" "$state_dir"
	# An earlier install's agent is unloaded first (an error when there is none, ignored).
	printf '  $ launchctl bootout %q 2>/dev/null || true\n' "gui/$UID/$label"
	if [ "$dry_run" = 0 ]; then
		launchctl bootout "gui/$UID/$label" 2>/dev/null || true
		printf '%s\n' "$content" >"$plist"
	fi
	run launchctl bootstrap "gui/$UID" "$plist"
	say "stop it: launchctl bootout gui/$UID/$label"
	say "remove it: launchctl bootout gui/$UID/$label; rm $plist"
else
	unit="$HOME/.config/systemd/user/factory-agent.service"
	say "installing supervise as the systemd user service factory-agent ($unit), logging to $log"
	content="[Unit]
Description=factory-agent supervise (ContainerSolutions/factory)
After=network-online.target

[Service]
ExecStart=$agent supervise
Restart=on-failure
RestartSec=60
# The first SIGTERM drains (the running task finishes); a second would abort it.
TimeoutStopSec=15min
StandardOutput=append:$log
StandardError=append:$log

[Install]
WantedBy=default.target"
	run mkdir -p "$HOME/.config/systemd/user" "$state_dir"
	if [ "$dry_run" = 0 ]; then
		printf '%s\n' "$content" >"$unit"
	fi
	run systemctl --user daemon-reload
	run systemctl --user enable --now factory-agent.service
	say "to keep it running while you are logged out: loginctl enable-linger $(id -un)"
	say "stop it: systemctl --user stop factory-agent"
	say "remove it: systemctl --user disable --now factory-agent; rm $unit; systemctl --user daemon-reload"
fi

say "done. $agent host check shows the host's state; supervise's log is $log."
say "uninstall: remove the service (above), then rm $agent${agent_linux:+ $agent_linux}; the config is $config_dir and the state $state_dir."
