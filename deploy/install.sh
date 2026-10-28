#!/usr/bin/env bash
# Sets up this machine as a factory agent host from a GitHub release of ContainerSolutions/factory
# (deploy/poc/README.md §Agent onboarding): an Apple-silicon Mac (tart) or a Linux x86-64 host
# (kata). Every release carries it, so
#
#   curl -fsSL https://github.com/ContainerSolutions/factory/releases/latest/download/install.sh | bash
#
# installs the latest release, asking for each setting (flags go after `| bash -s --`). It
# downloads factory-agent (and, on a Mac, factory-agent-linux-arm64 for the VMs) and checks them
# against SHA256SUMS; installs them to ~/.local/bin; writes ~/.config/factory-agent/agent.yaml
# and the enrollment token; runs factory-agent host init; offers the model logins; and installs
# supervise as a launchd user agent (Mac) or a systemd user service (Linux). Every value is
# checked before anything changes. Run it as the owner's own user, not root. Prompts read
# /dev/tty: under curl | bash, stdin is this script.
#
# Modes:
#   (default)     ask for each setting no flag or environment variable gives, showing its
#                 default (Enter takes it), then show agent.yaml and ask before writing it
#   --quiet       ask nothing: each setting is its flag, its environment variable or its default.
#                 The dispatcher's URL and the enrollment token have no default: without them it
#                 fails before changing anything. The model logins are left for later (it prints
#                 the commands), and host init gets --yes.
#   --dry-run     print every step and command; change nothing, download nothing
#   --no-service  don't install supervise as a service
#   --force       replace an existing agent.yaml (copied to agent.yaml.bak-<UTC time> first).
#                 Without it, --quiet keeps the file and the default mode asks (default: keep).
#                 A kept agent.yaml is not rewritten; the other steps still run.
#
# Settings: a flag (--name VALUE or --name=VALUE), else its environment variable, else the
# default. Lists are comma-separated. They make agent.yaml (deploy/poc/agent.yaml.example has
# every key):
#   --dispatcher URL        FACTORY_DISPATCHER      none: required. https, or http on a loopback host
#   --pool NAME             FACTORY_POOL            local
#   --host NAME             FACTORY_HOST            this machine's short host name, lower-cased
#   --harnesses LIST        FACTORY_HARNESSES       codex,claude-code (at least one of the two)
#   --codex-models LIST     FACTORY_CODEX_MODELS    gpt-6.1-sol
#   --codex-default MODEL   FACTORY_CODEX_DEFAULT   gpt-6.1-sol
#   --codex-efforts LIST    FACTORY_CODEX_EFFORTS   low,medium,high
#   --claude-models LIST    FACTORY_CLAUDE_MODELS   claude-sonnet-5-5,claude-opus-5-5
#   --claude-default MODEL  FACTORY_CLAUDE_DEFAULT  claude-sonnet-5-5
#   --claude-efforts LIST   FACTORY_CLAUDE_EFFORTS  low,medium,high,max
#   --max-concurrent N      FACTORY_MAX_CONCURRENT  1 (tasks at once; lending is unlimited)
#   --cpus N                FACTORY_CPUS            6 (each VM workspace's, 1 to 16)
#   --memory NGi            FACTORY_MEMORY          8Gi (2Gi to 64Gi)
#   --disk NGi              FACTORY_DISK            60Gi (20Gi to 500Gi)
# Efforts are from minimal, low, medium, high, max. A default model must be one of the models;
# when the default's default isn't, it is the list's first.
#
# The enrollment token, which an admin mints for this pool and host with `factory agent enroll`
# (it prints the command). Not needed when the host is already enrolled (the state dir holds a
# refresh token) or ~/.config/factory-agent/enrollment-token exists. Else, in this order:
#   --enrollment-token-file PATH   a file holding it (one line); it is copied, 0600
#   FACTORY_ENROLLMENT_TOKEN       the token itself
#   (default mode)                 asked for, not echoed
# No flag takes the token itself: it would show in ps and in the shell history. It is never
# printed.
#
# Other environment:
#   FACTORY_VERSION  the release tag to install (default: the latest release)
#   FACTORY_PREFIX   where the binaries go (default ~/.local/bin)
set -euo pipefail

repo_url="https://github.com/ContainerSolutions/factory"
label="com.containersolutions.factory-agent"

# The defaults: deploy/poc/agent.yaml.example's values (installsh_test.go checks they match, and
# that the header above lists them).
default_pool=local
default_harnesses=codex,claude-code
# The codex model: change it if your codex sign-in doesn't offer it (see /model in codex).
default_codex_models=gpt-6.1-sol
default_codex_default=gpt-6.1-sol
default_codex_efforts=low,medium,high
default_claude_models=claude-sonnet-5-5,claude-opus-5-5
default_claude_default=claude-sonnet-5-5
default_claude_efforts=low,medium,high,max
default_max_concurrent=1
default_cpus=6
default_memory=8Gi
default_disk=60Gi

# The settings that take a value (their flags with _ for -), and their values once resolved.
settings="dispatcher pool host harnesses codex_models codex_default codex_efforts claude_models claude_default claude_efforts max_concurrent cpus memory disk"
v_dispatcher="" v_pool="" v_host="" v_harnesses="" v_codex_models="" v_codex_default="" v_codex_efforts=""
v_claude_models="" v_claude_default="" v_claude_efforts="" v_max_concurrent="" v_cpus="" v_memory="" v_disk=""

dry_run=0
quiet=0
service=1
force=0
noask=0
token_path=""

die() {
	echo "install.sh: $*" >&2
	exit 1
}
say() { printf '==> %s\n' "$*"; }

usage() {
	if [ -f "${BASH_SOURCE[0]:-}" ]; then
		sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -e '/^set -euo/d' -e 's/^# \{0,1\}//'
	else
		echo "usage: install.sh [--quiet] [--dry-run] [--no-service] [--force] [--dispatcher URL] [--enrollment-token-file PATH]"
		echo "  [--pool NAME] [--host NAME] [--harnesses LIST] [--codex-models LIST] [--codex-default MODEL] [--codex-efforts LIST]"
		echo "  [--claude-models LIST] [--claude-default MODEL] [--claude-efforts LIST] [--max-concurrent N] [--cpus N] [--memory NGi] [--disk NGi]"
		echo "Each flag's default and environment variable: deploy/install.sh's header. No flag takes the enrollment token itself."
	fi
}

is_setting() {
	case " $settings " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

while [ $# -gt 0 ]; do
	arg="$1"
	val=""
	has_val=0
	case "$arg" in
	--*=*)
		val="${arg#*=}"
		arg="${arg%%=*}"
		has_val=1
		;;
	esac
	case "$arg" in
	--dry-run) dry_run=1 ;;
	--quiet) quiet=1 ;;
	--no-service) service=0 ;;
	--force) force=1 ;;
	--yes) die "--yes is replaced by --quiet" ;;
	-h | --help)
		usage
		exit 0
		;;
	--enrollment-token | --token) die "$arg: no flag takes the token itself (it would show in ps and the shell history): use --enrollment-token-file PATH or FACTORY_ENROLLMENT_TOKEN" ;;
	--*)
		name="${arg#--}"
		name="${name//-/_}"
		[ "$name" = enrollment_token_file ] || is_setting "$name" || die "unknown flag $arg (--help lists them)"
		if [ "$has_val" = 0 ]; then
			[ $# -ge 2 ] || die "$arg wants a value"
			val="$2"
			shift
		fi
		[ -n "$val" ] || die "$arg wants a value"
		if [ "$name" = enrollment_token_file ]; then
			token_path="$val"
			# shellcheck disable=SC2088 # a literal ~/ (from --enrollment-token-file=~/…) is the home
			case "$token_path" in "~/"*) token_path="$HOME/${token_path:2}" ;; esac
		else
			printf -v "flag_$name" '%s' "$val"
		fi
		;;
	*) die "unknown argument $arg (--help lists the flags)" ;;
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

# have_tty reports whether there is a terminal to ask on.
have_tty() { { : </dev/tty; } 2>/dev/null; }

# ask VAR PROMPT [silent]: reads a line from the terminal into VAR.
ask() {
	local var="$1" prompt="$2" silent="${3:-}" answer=""
	[ "$quiet" = 0 ] || die "--quiet: nothing may be asked ($prompt)"
	have_tty || die "no terminal to ask on: pass --quiet, with the settings as flags or environment variables"
	if [ -n "$silent" ]; then
		read -rs -p "$prompt" answer </dev/tty || die "no answer (the terminal closed)"
		echo >/dev/tty
	else
		read -r -p "$prompt" answer </dev/tty || die "no answer (the terminal closed)"
	fi
	printf -v "$var" '%s' "$answer"
}

# confirm PROMPT DEFAULT: y or n from the terminal, DEFAULT (y or n) on Enter; no with --quiet.
confirm() {
	local answer=""
	[ "$quiet" = 0 ] || return 1
	if [ "$2" = y ]; then
		ask answer "$1 [Y/n] "
		answer="${answer:-y}"
	else
		ask answer "$1 [y/N] "
	fi
	case "$answer" in y | Y | yes | YES) return 0 ;; *) return 1 ;; esac
}

# The checks. invalid NAME VALUE prints what is wrong with VALUE for the setting NAME, or nothing.
name_re='^[a-z0-9][a-z0-9-]*$'
model_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
gib_re='^[1-9][0-9]{0,5}Gi$'

# list_problem VALUE ITEM_RE [ALLOWED]: VALUE must be a comma-separated list of distinct items
# matching ITEM_RE and, given ALLOWED, each one of its space-separated words.
list_problem() {
	local v="$1" re="$2" allowed="${3:-}" item seen=" " items
	case "$v" in "" | ,* | *, | *,,*) echo "want a comma-separated list, without empty items" && return 0 ;; esac
	IFS=, read -r -a items <<<"$v"
	for item in "${items[@]}"; do
		if ! [[ "$item" =~ $re ]]; then
			echo "$item is not a valid name" && return 0
		fi
		if [ -n "$allowed" ]; then
			case " $allowed " in *" $item "*) ;; *) echo "$item is not one of $allowed" && return 0 ;; esac
		fi
		case "$seen" in *" $item "*) echo "$item is listed twice" && return 0 ;; esac
		seen="$seen$item "
	done
}

# in_list ITEM LIST: whether the comma-separated LIST holds ITEM.
in_list() {
	case ",$2," in *",$1,"*) return 0 ;; *) return 1 ;; esac
}

invalid() {
	local name="$1" v="$2" n lo=2 hi=64
	case "$name" in
	dispatcher)
		# The loader's rule: an absolute URL without credentials, query or fragment; plain http
		# only on a loopback host.
		[[ "$v" =~ ^https://([A-Za-z0-9][A-Za-z0-9.-]*|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$ ]] && return 0
		[[ "$v" =~ ^http://(localhost|127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|\[::1\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$ ]] && return 0
		echo "$v: want an https URL without credentials, query or fragment, or http on a loopback host (localhost, 127.x.x.x, [::1])"
		;;
	pool | host) [[ "$v" =~ $name_re ]] || echo "$v: want lower-case letters, digits and '-', starting with a letter or a digit" ;;
	harnesses) list_problem "$v" "$name_re" "codex claude-code" ;;
	codex_models | claude_models) list_problem "$v" "$model_re" ;;
	codex_efforts | claude_efforts) list_problem "$v" "$name_re" "minimal low medium high max" ;;
	codex_default) in_list "$v" "$v_codex_models" || echo "$v is not one of the models ($v_codex_models)" ;;
	claude_default) in_list "$v" "$v_claude_models" || echo "$v is not one of the models ($v_claude_models)" ;;
	max_concurrent) [[ "$v" =~ ^[1-9][0-9]{0,2}$ ]] || echo "$v: want a whole number from 1 to 999" ;;
	cpus) [[ "$v" =~ ^([1-9]|1[0-6])$ ]] || echo "$v: want a whole number from 1 to 16" ;;
	memory | disk)
		[ "$name" = memory ] || lo=20 hi=500
		if [[ "$v" =~ $gib_re ]]; then
			n="${v%Gi}"
			[ "$n" -lt "$lo" ] || [ "$n" -gt "$hi" ] || return 0
		fi
		echo "$v: want ${lo}Gi to ${hi}Gi (a whole number of Gi)"
		;;
	esac
}

env_of() { printf 'FACTORY_%s' "$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')"; }
flag_of() { printf -- '--%s' "${1//_/-}"; }

# given NAME prints the setting's flag value, else its environment variable's (nothing when
# neither is set).
given() {
	local f="flag_$1" e
	e="$(env_of "$1")"
	if [ -n "${!f:-}" ]; then
		printf '%s' "${!f}"
	else
		printf '%s' "${!e:-}"
	fi
}

# setting NAME DEFAULT PROMPT sets v_NAME: the flag's or the environment's value, else (in the
# default mode) the terminal's answer, Enter taking DEFAULT, else DEFAULT. A given value that
# fails its check is an error naming the flag; a bad answer is asked again. Without a DEFAULT
# the setting is required.
setting() {
	local name="$1" def="$2" prompt="$3" v problem
	v="$(given "$name")"
	if [ -n "$v" ]; then
		problem="$(invalid "$name" "$v")"
		[ -z "$problem" ] || die "$(flag_of "$name") ($(env_of "$name")): $problem"
	elif [ "$quiet" = 1 ] || [ "$noask" = 1 ]; then
		v="$def"
	else
		while :; do
			if [ -n "$def" ]; then
				ask v "$prompt [$def]: "
				v="${v:-$def}"
			else
				ask v "$prompt: "
			fi
			v="${v// /}"
			[ -n "$v" ] || continue
			problem="$(invalid "$name" "$v")"
			[ -n "$problem" ] || break
			echo "  $problem" >&2
		done
	fi
	printf -v "v_$name" '%s' "$v"
}

# default_model LIST DEFAULT: DEFAULT when LIST holds it, else LIST's first.
default_model() {
	if in_list "$2" "$1"; then printf '%s' "$2"; else printf '%s' "${1%%,*}"; fi
}

# yaml_list LIST: a comma-separated list as a YAML flow sequence.
yaml_list() { printf '[%s]' "${1//,/, }"; }

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

version="${FACTORY_VERSION:-}"
if [ -n "$version" ]; then
	[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "FACTORY_VERSION $version is not a vX.Y.Z tag"
	base_url="$repo_url/releases/download/$version"
else
	version=latest
	base_url="$repo_url/releases/latest/download"
fi

# 2. The settings and the enrollment token, all checked before anything changes.
config_dir="$HOME/.config/factory-agent"
config="$config_dir/agent.yaml"
token_file="$config_dir/enrollment-token"
refresh_file="$state_dir/agent-refresh-token"

host_default="$(uname -n)"
host_default="${host_default%%.*}"
host_default="$(printf '%s' "$host_default" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')"
host_default="${host_default#"${host_default%%[!-]*}"}"
host_default="${host_default:-agent}"

write_config=1
if [ -e "$config" ]; then
	if [ "$force" = 1 ]; then
		say "$config exists: --force replaces it, after a copy"
	elif [ "$quiet" = 1 ] || ! confirm "$config exists. Replace it (after a copy to agent.yaml.bak-<UTC time>)?" n; then
		write_config=0
		say "$config exists: keeping it, so the config settings are not used (--force replaces it)"
	fi
fi

# Where the enrollment token comes from: enrolled (a refresh token), tokenfile (the token file
# exists), path (--enrollment-token-file), environment, ask, or missing.
if [ -e "$refresh_file" ]; then
	token_from=enrolled
elif [ -e "$token_file" ]; then
	token_from=tokenfile
elif [ -n "$token_path" ]; then
	token_from=path
elif [ -n "${FACTORY_ENROLLMENT_TOKEN:-}" ]; then
	token_from=environment
elif [ "$quiet" = 0 ]; then
	token_from=ask
else
	token_from=missing
fi
case "$token_from" in enrolled | tokenfile) [ -z "$token_path" ] || say "--enrollment-token-file is not used: no enrollment token is needed" ;; esac

if [ "$quiet" = 1 ]; then
	missing=""
	if [ "$write_config" = 1 ] && [ -z "$(given dispatcher)" ]; then
		missing="the dispatcher's URL (--dispatcher URL or FACTORY_DISPATCHER)"
	fi
	if [ "$token_from" = missing ]; then
		pool_hint="$(given pool)" host_hint="$(given host)"
		[ -z "$missing" ] || missing="$missing and "
		missing="${missing}the enrollment token (--enrollment-token-file PATH or FACTORY_ENROLLMENT_TOKEN; an admin mints it with: factory agent enroll --pool ${pool_hint:-$default_pool} --owner <your GitHub login> --name ${host_hint:-$host_default})"
	fi
	[ -z "$missing" ] || die "--quiet: missing $missing; nothing was changed"
fi

# With the config kept, only the enrollment prompt needs the pool and the host.
[ "$write_config" = 1 ] || [ "$token_from" = ask ] || noask=1
if [ "$write_config" = 1 ]; then
	setting dispatcher "" "The dispatcher's URL (https://…)"
fi
setting pool "$default_pool" "Pool"
setting host "$host_default" "Host name"
if [ "$write_config" = 1 ]; then
	setting harnesses "$default_harnesses" "Harnesses (codex, claude-code)"
	yaml_harnesses=""
	if in_list codex "$v_harnesses"; then
		setting codex_models "$default_codex_models" "codex models"
		setting codex_default "$(default_model "$v_codex_models" "$default_codex_default")" "codex default model"
		setting codex_efforts "$default_codex_efforts" "codex efforts (from minimal, low, medium, high, max)"
		yaml_harnesses="$yaml_harnesses
  codex:
    # Change the model if your codex sign-in doesn't offer it (see /model in codex).
    models: $(yaml_list "$v_codex_models")
    efforts: $(yaml_list "$v_codex_efforts")
    default: $v_codex_default"
	fi
	if in_list claude-code "$v_harnesses"; then
		setting claude_models "$default_claude_models" "claude-code models"
		setting claude_default "$(default_model "$v_claude_models" "$default_claude_default")" "claude-code default model"
		setting claude_efforts "$default_claude_efforts" "claude-code efforts (from minimal, low, medium, high, max)"
		yaml_harnesses="$yaml_harnesses
  claude-code:
    models: $(yaml_list "$v_claude_models")
    efforts: $(yaml_list "$v_claude_efforts")
    default: $v_claude_default"
	fi
	setting max_concurrent "$default_max_concurrent" "Tasks at once"
	setting cpus "$default_cpus" "CPUs per VM workspace"
	setting memory "$default_memory" "Memory per VM workspace"
	setting disk "$default_disk" "Disk per VM workspace"

	yaml="# factory-agent's config, written by install.sh. Every key: deploy/poc/agent.yaml.example.
dispatcher: $v_dispatcher
pool: $v_pool
host: $v_host
enrollmentTokenFile: ~/.config/factory-agent/enrollment-token
# A harness is offered when its login is stored (factory-agent login).
harnesses:$yaml_harnesses
lending:
  unlimited: true
  maxConcurrent: $v_max_concurrent
workspace:
  size: { cpus: $v_cpus, memory: $v_memory, disk: $v_disk }"
	printf -- '--- agent.yaml ---\n%s\n--- end agent.yaml ---\n' "$yaml"
	if [ "$quiet" = 0 ] && [ "$dry_run" = 0 ] && ! confirm "Write it?" y; then
		die "not written; nothing was changed"
	fi
fi

token=""
case "$token_from" in
enrolled) say "this host is already enrolled ($refresh_file exists): no enrollment token needed" ;;
tokenfile) say "$token_file exists: using it" ;;
path)
	[ -f "$token_path" ] && [ -r "$token_path" ] || die "--enrollment-token-file $token_path: not a readable file"
	[ "$(grep -c '[^[:space:]]' "$token_path" || true)" = 1 ] || die "--enrollment-token-file $token_path: want the token on a single line"
	token="$(grep '[^[:space:]]' "$token_path" | tr -d '[:space:]')"
	;;
environment) token="$FACTORY_ENROLLMENT_TOKEN" ;;
ask)
	say "this host is $v_host. An admin of the dispatcher mints its enrollment token with"
	say "  factory agent enroll --pool $v_pool --owner <your GitHub login> --name $v_host"
	if [ "$dry_run" = 1 ]; then
		say "dry run: the token would be asked for here (not echoed)"
	else
		while [ -z "$token" ]; do
			ask token "Paste the enrollment token (not echoed): " silent
		done
	fi
	;;
esac
case "$token" in *[[:space:]]*) die "the enrollment token must be a single line without spaces" ;; esac
case "$token_from" in path | environment) [ -n "$token" ] || die "an empty enrollment token" ;; esac

# 3. The release, the downloads and their checksums.
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

# 4. Install.
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

# 5. The config and the enrollment token.
run mkdir -p -m 0700 "$config_dir"
if [ "$write_config" = 1 ]; then
	if [ -e "$config" ]; then
		bak="$config.bak-$(date -u +%Y%m%dT%H%M%SZ)"
		say "copying $config to $bak (0600)"
		if [ "$dry_run" = 0 ]; then
			(umask 077 && cp "$config" "$bak" && chmod 600 "$bak")
		fi
	fi
	say "writing $config (0600)"
	if [ "$dry_run" = 0 ]; then
		(umask 077 && printf '%s\n' "$yaml" >"$config.tmp" && chmod 600 "$config.tmp" && mv -f "$config.tmp" "$config")
	fi
fi
if [ -n "$token" ]; then
	say "writing $token_file (0600)"
	if [ "$dry_run" = 0 ]; then
		(umask 077 && printf '%s\n' "$token" >"$token_file")
	fi
fi
unset token FACTORY_ENROLLMENT_TOKEN

# 6. The host: host init asks for sudo itself.
init=("$agent" host init --driver "$driver")
[ "$driver" != tart ] || init+=(--agent-linux "$agent_linux")
[ "$quiet" = 0 ] || init+=(--yes)
say "host init (it asks for sudo once; the VM images take a while)"
if [ "$dry_run" = 1 ]; then
	run "${init[@]}"
elif have_tty; then
	run "${init[@]}" </dev/tty
else
	run "${init[@]}" </dev/null
fi

# 7. The model logins, which only the gateway holds.
say "the model logins (each skippable; later: $agent login claude|codex)"
if [ "$dry_run" = 0 ] && confirm "Store a claude login now (paste the token claude setup-token printed)?" n; then
	claude_token=""
	ask claude_token "Paste the claude setup-token token (not echoed): " silent
	printf '%s\n' "$claude_token" | "$agent" login claude
	unset claude_token
else
	say "  claude: run claude setup-token on its own, copy the token, then: $agent login claude"
fi
if [ "$dry_run" = 0 ] && confirm "Import the codex login (after codex login on this machine)?" n; then
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
