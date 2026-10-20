#!/usr/bin/env bash
# Starts the PoC dispatcher (deploy/poc/README.md): loads deploy/poc/.env, checks it, builds
# bin/factory with the real UI (make build-release) unless SKIP_BUILD=1, then execs
# factory serve with its JSON log appended to deploy/poc/logs/ and copied to the terminal.
# Run it from anywhere (make poc-up runs it from the repo root); extra arguments go to
# factory serve (e.g. --log-level debug). It never prints a value from .env.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
env_file="$here/.env"
log_dir="$here/logs"

die() {
	echo "run.sh: $*" >&2
	exit 1
}

# The file holds the App keys: it must be a regular file of ours that nobody else can read.
[ -f "$env_file" ] || die "$env_file is missing: cp deploy/poc/.env.example deploy/poc/.env, chmod 600 it and fill it in (README §Secrets)"
[ -O "$env_file" ] || die "$env_file is not owned by $(id -un)"
mode="$(stat -c %a "$env_file" 2>/dev/null || stat -f %Lp "$env_file")"
[ "$mode" = 600 ] || die "$env_file has mode $mode: chmod 600 it"

# Sourced with allexport, so every assignment reaches factory serve. Tracing stays off: it
# would print the values.
set +x
set -a
# shellcheck source=/dev/null
. "$env_file"
set +a

# What factory serve needs for the PoC: its own variables and every catalog secretRef
# (.env.example lists them). Names only in the error.
required=(
	FACTORY_PUBLIC_URL
	FACTORY_OAUTH_CLIENT_ID
	FACTORY_OAUTH_CLIENT_SECRET
	FACTORY_SESSION_KEY
	FACTORY_CLI_TOKEN
	FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET
	FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY
	FACTORY_SECRET_CCF_FACTORY_REVIEWER_APP_PRIVATE_KEY
	FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN
)
missing=()
for name in "${required[@]}"; do
	[ -n "${!name:-}" ] || missing+=("$name")
done
[ "${#missing[@]}" -eq 0 ] || die "not set in $env_file: ${missing[*]}"
case "$FACTORY_PUBLIC_URL" in
https://*) ;;
*) die "FACTORY_PUBLIC_URL must be the https tunnel URL (the UI's session cookies are Secure)" ;;
esac

cd "$root"
if [ "${SKIP_BUILD:-}" = 1 ]; then
	[ -x bin/factory ] || die "SKIP_BUILD=1 but bin/factory is missing: run make build-release"
else
	make build-release
fi

# The catalog: a local directory (FACTORY_CATALOG_DIR), or catalog/ of
# compliance-framework/factory at FACTORY_CATALOG_REF (default main), polled every minute.
if [ -n "${FACTORY_CATALOG_DIR:-}" ]; then
	catalog=(--catalog-dir "$FACTORY_CATALOG_DIR")
else
	catalog=(--catalog-repo https://github.com/compliance-framework/factory
		--catalog-ref "${FACTORY_CATALOG_REF:-main}" --catalog-path catalog)
fi

umask 077
mkdir -p "$log_dir"
log="$log_dir/factory-serve-$(date -u +%Y%m%dT%H%M%SZ).log"
echo "run.sh: logging to $log" >&2

# tee ignores Ctrl-C and SIGTERM, so it keeps copying factory serve's shutdown logs and exits
# once serve closes its output.
exec > >(
	trap '' INT TERM
	exec tee -a "$log"
) 2>&1
exec bin/factory serve \
	--addr "${FACTORY_ADDR:-127.0.0.1:8080}" \
	--ops-addr 127.0.0.1:9090 \
	--public-url "$FACTORY_PUBLIC_URL" \
	"${catalog[@]}" \
	--retention 2160h \
	--log-format json \
	"$@"
