#!/usr/bin/env bash
# Demo: the TOML config file (issue #84), on running hubs.
#
# It shows: a hub booted from a file alone; each rung of the precedence ladder
# (file, overridden by an environment variable, overridden by a flag) observed
# on a running hub, for a listen address, a log level, and the admin token; a
# secret read through file: from the config file, relative to the file's own
# directory, that works against the Admin API and appears nowhere in the hub's
# output; an unknown key, a wrong type, a duplicate key, a syntax error, a
# missing secret file, and an empty one each refusing to start with a message
# that names the key and no value; the file never read unless named; the
# storage command taking db from the same file; and `vyshka-hub config`
# printing what a hub would use, with where each value came from and every
# secret redacted, with a control that shows the redaction check can fail.
#
# It boots its own short-lived hubs, each from its own working directory away
# from the config files, and ignores the e2e hub. The VYSHKA_* variables the
# e2e script exports are dropped first, since a demo of precedence must start
# from an empty environment. Set CONFIG_DEMO_KEEP=1 to keep the run directory.
#
# Needs Go and curl.
set -euo pipefail

cd "$(dirname "$0")/.."

for name in $(compgen -e | grep -E '^(VYSHKA_|DATABASE_URL$)' || true); do unset "$name"; done

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/vyshka-config.XXXXXX")"
exe=""
case "$(go env GOOS)" in windows) exe=.exe ;; esac
# The hubs are native programs: on Windows they need Windows paths.
native() { if command -v cygpath > /dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

hub_pid=""
cleanup() {
  [ -z "$hub_pid" ] || { kill "$hub_pid" 2>/dev/null || true; wait "$hub_pid" 2>/dev/null || true; }
  if [ "${CONFIG_DEMO_KEEP:-}" = 1 ]; then echo "run directory kept: $run_dir"; else rm -rf "$run_dir"; fi
}
trap cleanup EXIT

step() { printf '\n--- %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
stop() { [ -z "$hub_pid" ] || { kill "$hub_pid" 2>/dev/null || true; wait "$hub_pid" 2>/dev/null || true; hub_pid=""; }; }
expect() { # expect LABEL GOT WANT
  printf '  %-58s %s\n' "$1" "$2"
  [ "$2" = "$3" ] || fail "$1: got $2, want $3"
}
random() { od -An -N12 -tx1 /dev/urandom | tr -d ' \n'; }

base=$((23000 + (RANDOM % 600) * 10))
port_file=$base; port_env=$((base + 1)); port_flag=$((base + 2)); port_metrics=$((base + 3))
url() { printf 'http://127.0.0.1:%s' "$1"; }

# The binary carries the port in its name, so nothing here can stop another hub.
bin="$run_dir/vyshka-hub-config-$base$exe"
step "Build the hub"
go build -o "$bin" ./hub/cmd/vyshka-hub
"$bin" version

etc="$run_dir/etc"; away="$run_dir/away"
mkdir -p "$etc/secrets" "$away"
token_file="vya_file_$(random)"; token_env="vya_env_$(random)"; token_flag="vya_flag_$(random)"
printf '%s\n\n' "$token_file" > "$etc/secrets/admin-token"   # a trailing newline, as editors leave
cfg="$etc/hub.toml"
cat > "$cfg" <<EOF
# The shared file: every value a hub needs, the token by reference.
addr = "127.0.0.1:$port_file"
db = "$(native "$run_dir/a.db")"
admin-token = "file:secrets/admin-token"
log-level = "debug"
metrics-addr = "127.0.0.1:$port_metrics"
backup-keep = 3
EOF
chmod 600 "$cfg"
echo "config file:"; sed 's/^/  | /' "$cfg"
echo "secret file: $etc/secrets/admin-token (referenced as file:secrets/admin-token, so relative to the config file)"
cd "$away"  # every hub below runs from here, which holds no config

wait_healthy() { # wait_healthy PORT LOG
  local i
  for i in $(seq 1 100); do
    curl --silent --fail "$(url "$1")/healthz" > /dev/null 2>&1 && return 0
    kill -0 "$hub_pid" 2>/dev/null || { cat "$2"; fail "the hub exited during boot"; }
    sleep 0.1
  done
  cat "$2"; fail "no /healthz on port $1"
}
status() { # status PORT [TOKEN]: the Admin API's answer to a servers listing, 000 when nothing listens
  local args=(--silent --output /dev/null --max-time 3 --write-out '%{http_code}')
  [ -z "${2:-}" ] || args+=(-H "Authorization: Bearer $2")
  curl "${args[@]}" "$(url "$1")/api/v1/servers" || true
}
info_lines() { grep -c '"level":"INFO"' "$1" || true; }

step "1. A hub booted from the file alone: hub -config hub.toml, no flags, no environment"
"$bin" serve -config "$(native "$cfg")" > "$run_dir/hub-file.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_file" "$run_dir/hub-file.log"
expect "listens on the file's addr ($port_file): /healthz" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_file")/healthz")" 200
expect "the metrics listener from the file ($port_metrics)" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_metrics")/metrics")" 200
expect "Admin API with the token read through file: " "$(status "$port_file" "$token_file")" 200
expect "Admin API with no token" "$(status "$port_file")" 401
expect "Admin API with a wrong token" "$(status "$port_file" "$token_env")" 401
expect "log-level debug from the file: INFO lines in the log" "$([ "$(info_lines "$run_dir/hub-file.log")" -gt 0 ] && echo yes || echo no)" yes
grep '"config file"' "$run_dir/hub-file.log" | sed 's/^/  log: /'
stop
echo "  the same database file the file named: $(ls "$run_dir"/a.db* | head -n 1 | sed "s|$run_dir/||")"

step "2. The environment overrides the file: VYSHKA_ADDR, VYSHKA_LOG_LEVEL, VYSHKA_ADMIN_TOKEN"
VYSHKA_ADDR="127.0.0.1:$port_env" VYSHKA_LOG_LEVEL=error VYSHKA_ADMIN_TOKEN="$token_env" \
  "$bin" serve -config "$(native "$cfg")" > "$run_dir/hub-env.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_env" "$run_dir/hub-env.log"
expect "listens on the environment's port ($port_env)" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_env")/healthz")" 200
expect "nothing on the file's port ($port_file)" "$(status "$port_file" "$token_file")" 000
expect "the file's token is refused" "$(status "$port_env" "$token_file")" 401
expect "the environment's token works" "$(status "$port_env" "$token_env")" 200
expect "log-level error from the environment: INFO lines in the log" "$(info_lines "$run_dir/hub-env.log")" 0
stop

step "3. A flag overrides the environment and the file"
VYSHKA_ADDR="127.0.0.1:$port_env" VYSHKA_ADMIN_TOKEN="$token_env" \
  "$bin" serve -config "$(native "$cfg")" -addr "127.0.0.1:$port_flag" -admin-token "$token_flag" -log-level info \
  > "$run_dir/hub-flag.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_flag" "$run_dir/hub-flag.log"
expect "listens on the flag's port ($port_flag)" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_flag")/healthz")" 200
expect "nothing on the environment's port ($port_env)" "$(status "$port_env" "$token_env")" 000
expect "nothing on the file's port ($port_file)" "$(status "$port_file" "$token_file")" 000
expect "the environment's token is refused" "$(status "$port_flag" "$token_env")" 401
expect "the file's token is refused" "$(status "$port_flag" "$token_file")" 401
expect "the flag's token works" "$(status "$port_flag" "$token_flag")" 200
expect "log-level info from the flag: INFO lines in the log" "$([ "$(info_lines "$run_dir/hub-flag.log")" -gt 0 ] && echo yes || echo no)" yes
stop

step "4. No secret reached any hub's output"
for secret in "$token_file" "$token_env" "$token_flag"; do
  if grep -q "$secret" "$run_dir"/hub-*.log; then fail "a hub logged $secret"; fi
done
echo "  none of the three tokens appears in $(ls "$run_dir"/hub-*.log | wc -l | tr -d ' ') hub logs"
# The control: the same grep does find the token in the one place it is written.
grep -q "$token_file" "$etc/secrets/admin-token" || fail "the control grep cannot see the token"
echo "  (control: the same grep finds the file token in its secret file)"

step "5. A bad file stops serve before it listens, naming the key and the file, never a value"
refuse() { # refuse LABEL CONTENTS [SECRET-FILE-CONTENTS]
  local file="$etc/bad-$RANDOM.toml" out rc=0
  printf '%b' "$2" > "$file"
  out="$("$bin" serve -config "$(native "$file")" -addr "127.0.0.1:$port_flag" 2>&1)" || rc=$?
  printf '  %-26s exit %s: %s\n' "$1" "$rc" "$(printf '%s' "$out" | tail -n 1 | sed "s|$(native "$etc")/||; s|$etc/||")"
  [ "$rc" != 0 ] || fail "$1: serve accepted the file"
  case "$out" in *DEMO_VALUE*) fail "$1: the message carries a value" ;; esac
  case "$out" in *"$(basename "$file")"*) ;; *) fail "$1: the message does not name the file" ;; esac
  printf '%s' "$out" > "$run_dir/last-refusal.txt"
}
refuse "unknown key" 'adress = "DEMO_VALUE"\n'
grep -q 'unknown key "adress"' "$run_dir/last-refusal.txt" || fail "the unknown key is not named"
refuse "wrong type" 'backup-keep = "DEMO_VALUE"\n'
grep -q 'key "backup-keep" is a string, want an integer' "$run_dir/last-refusal.txt" || fail "the wrong type is not named"
refuse "duplicate key" 'addr = "DEMO_VALUE"\naddr = "DEMO_VALUE"\n'
grep -q 'key "addr" is set twice' "$run_dir/last-refusal.txt" || fail "the duplicate is not named"
refuse "unquoted secret" 'admin-token = DEMO_VALUE\n'
refuse "unreadable secret file" 'admin-token = "file:secrets/missing"\n'
grep -q 'admin-token (from config file' "$run_dir/last-refusal.txt" || fail "the setting is not named"
printf ' \n' > "$etc/secrets/empty"
refuse "empty secret file" 'admin-token = "file:secrets/empty"\n'
grep -Eq 'secrets.empty is empty' "$run_dir/last-refusal.txt" || fail "the empty file is not named"
echo "  (every message above names the file; none carries the DEMO_VALUE each file held)"

step "6. A file next to the hub is never read unless named"
printf 'addr = "127.0.0.1:1"\n' > "$away/hub.toml"; cp "$away/hub.toml" "$away/vyshka.toml"
"$bin" config | sed -n '1p;/^addr /p'
"$bin" config | grep -q '^addr .*127.0.0.1:8080 .*default' || fail "an unnamed file was read"
rm -f "$away/hub.toml" "$away/vyshka.toml"

step "7. vyshka-hub config: what the hub will use, and from where (flags, environment, file, default)"
secret_db="DEMO_DB_PASSWORD_$(random)"; secret_inline="vya_DEMO_INLINE_$(random)"
cat > "$etc/show.toml" <<EOF
db = "postgres://vyshka:$secret_db@db.example:5432/vyshka"
admin-token = "$secret_inline"
log-level = "debug"
maps-dir = ""
backup-keep = 3
auto-tls-hosts = ["hub.example.org", "hub2.example.org"]
auto-tls = true
auto-tls-cache = "/var/lib/vyshka/acme-cache"
EOF
chmod 600 "$etc/show.toml"
show() { VYSHKA_LOG_LEVEL=warn VYSHKA_ADMIN_TOKEN="$token_env" "$bin" config -config "$(native "$etc/show.toml")" -addr 127.0.0.1:9000 -panel=false "$@" 2>&1; }
show > "$run_dir/show.txt"
sed 's/^/  /' "$run_dir/show.txt"
for secret in "$secret_db" "$secret_inline" "$token_env"; do
  if grep -q "$secret" "$run_dir/show.txt"; then fail "the config output carries a secret"; fi
done
grep -q "$secret_db" "$etc/show.toml" || fail "the control grep cannot see the password"
grep -q "$secret_inline" "$etc/show.toml" || fail "the control grep cannot see the inline token"
echo "  none of the three secrets is in that output (control: the same grep finds the password and the inline token in show.toml)"
grep -qE '^db +<redacted> +file$' "$run_dir/show.txt" || fail "db is not shown as redacted from the file"
grep -qE '^admin-token +<redacted> +env VYSHKA_ADMIN_TOKEN$' "$run_dir/show.txt" || fail "the token is not redacted from the environment"
grep -qE '^log-level +warn +env VYSHKA_LOG_LEVEL$' "$run_dir/show.txt" || fail "env does not beat the file for log-level"
grep -qE '^backup-keep +3 +file$' "$run_dir/show.txt" || fail "backup-keep is not from the file"
grep -qE '^addr +127.0.0.1:9000 +flag$' "$run_dir/show.txt" || fail "addr is not from the flag"
grep -qE '^panel +false +flag$' "$run_dir/show.txt" || fail "panel is not from the flag"

step "8. The same command runs serve's checks: an unreadable secret, an incoherent auto-TLS combination"
rc=0; out="$("$bin" config -config "$(native "$etc/show.toml")" -admin-token file:/nonexistent/token 2>&1)" || rc=$?
printf '  exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1)"
[ "$rc" != 0 ] || fail "an unreadable secret passed the check"
printf 'auto-tls-email = "ops@example.org"\n' > "$etc/tls.toml"
rc=0; out="$("$bin" config -config "$(native "$etc/tls.toml")" 2>&1)" || rc=$?
printf '  exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1 | cut -c1-110)"
[ "$rc" != 0 ] || fail "an incoherent auto-TLS combination passed the check"

step "9. A secret written into a file that group or others can read earns one warning (not on Windows)"
if [ "$(go env GOOS)" = windows ]; then
  echo "  skipped: Windows has no group or other permission bits"
else
  chmod 644 "$etc/show.toml"
  warn="$("$bin" config -config "$etc/show.toml" 2>&1 >/dev/null | grep -c '^warning:' || true)"
  expect "warnings for a 0644 file holding two inline secrets" "$warn" 2
  "$bin" config -config "$etc/show.toml" 2>&1 >/dev/null | sed 's/^/  /'
  everything="$("$bin" config -config "$etc/show.toml" 2>&1)"
  case "$everything" in *"$secret_db"*|*"$secret_inline"*) fail "a warning carries a secret" ;; esac
  chmod 600 "$etc/show.toml"
  expect "warnings once the file is 0600" "$("$bin" config -config "$etc/show.toml" 2>&1 >/dev/null | grep -c '^warning:' || true)" 0
fi

step "10. The storage command takes db from the same file (db only; the other keys are checked, not used)"
"$bin" storage -config "$(native "$cfg")" | head -n 2 | sed 's/^/  /'
printf 'dbb = "x.db"\n' > "$etc/typo.toml"
rc=0; out="$("$bin" storage -config "$(native "$etc/typo.toml")" 2>&1)" || rc=$?
printf '  a typo in the shared file: exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1 | sed "s|$(native "$etc")/||")"
[ "$rc" != 0 ] || fail "storage accepted a file with an unknown key"

echo
echo "OK: the config file works as designed on running hubs"
