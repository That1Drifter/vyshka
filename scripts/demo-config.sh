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
# storage command taking db from the same file; the old SQLite URI spelling, DATABASE_URL=file:<database>,
# refused without echoing the database; and `vyshka-hub config`
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
log-level = "error"
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
# The default level (info) logs a line per request and one at boot; error logs neither.
expect "log-level error from the file: INFO lines in the log" "$(info_lines "$run_dir/hub-file.log")" 0
stop
expect "the database the file named (a.db) was created" "$([ -e "$run_dir/a.db" ] && echo yes || echo no)" yes
expect "the default database (vyshka.db) was not" "$([ -e "$away/vyshka.db" ] && echo yes || echo no)" no

step "2. The environment overrides the file: VYSHKA_ADDR, VYSHKA_LOG_LEVEL, VYSHKA_ADMIN_TOKEN"
VYSHKA_ADDR="127.0.0.1:$port_env" VYSHKA_LOG_LEVEL=info VYSHKA_ADMIN_TOKEN="$token_env" \
  "$bin" serve -config "$(native "$cfg")" > "$run_dir/hub-env.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_env" "$run_dir/hub-env.log"
expect "listens on the environment's port ($port_env)" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_env")/healthz")" 200
expect "nothing on the file's port ($port_file)" "$(status "$port_file" "$token_file")" 000
expect "the file's token is refused" "$(status "$port_env" "$token_file")" 401
expect "the environment's token works" "$(status "$port_env" "$token_env")" 200
expect "log-level info from the environment: INFO lines in the log" "$([ "$(info_lines "$run_dir/hub-env.log")" -gt 0 ] && echo yes || echo no)" yes
grep '"config file"' "$run_dir/hub-env.log" | sed 's/^/  log: /'
stop

step "2b. An empty variable counts as unset: how a container image's own VYSHKA_ADDR and DATABASE_URL are released to the file"
VYSHKA_ADDR="" DATABASE_URL="" VYSHKA_ADMIN_TOKEN="" "$bin" serve -config "$(native "$cfg")" > "$run_dir/hub-empty.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_file" "$run_dir/hub-empty.log"
expect "listens on the file's port ($port_file) with the variables set empty" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_file")/healthz")" 200
expect "the file's token works (an empty token variable is unset too)" "$(status "$port_file" "$token_file")" 200
stop
# The same variables set non-empty, as the image sets them, outrank the file.
VYSHKA_ADDR="127.0.0.1:$port_env" DATABASE_URL="$(native "$run_dir/image.db")" "$bin" serve -config "$(native "$cfg")" > "$run_dir/hub-image.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_env" "$run_dir/hub-image.log"
expect "with them set, the variables win: the environment's port answers" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_env")/healthz")" 200
expect "and the environment's database was created, not the file's" "$([ -e "$run_dir/image.db" ] && echo yes || echo no)" yes
stop

step "3. A flag overrides the environment and the file"
VYSHKA_ADDR="127.0.0.1:$port_env" VYSHKA_ADMIN_TOKEN="$token_env" VYSHKA_LOG_LEVEL=info \
  "$bin" serve -config "$(native "$cfg")" -addr "127.0.0.1:$port_flag" -admin-token "$token_flag" -log-level error \
  > "$run_dir/hub-flag.log" 2>&1 &
hub_pid=$!
wait_healthy "$port_flag" "$run_dir/hub-flag.log"
expect "listens on the flag's port ($port_flag)" "$(curl -s -o /dev/null -w '%{http_code}' "$(url "$port_flag")/healthz")" 200
expect "nothing on the environment's port ($port_env)" "$(status "$port_env" "$token_env")" 000
expect "nothing on the file's port ($port_file)" "$(status "$port_file" "$token_file")" 000
expect "the environment's token is refused" "$(status "$port_flag" "$token_env")" 401
expect "the file's token is refused" "$(status "$port_flag" "$token_file")" 401
expect "the flag's token works" "$(status "$port_flag" "$token_flag")" 200
expect "log-level error from the flag over the environment's info: INFO lines" "$(info_lines "$run_dir/hub-flag.log")" 0
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
# refuse LABEL CONTENTS: serve must exit by itself. It runs in the background with its PID
# recorded, so a file that was wrongly accepted fails the demo after 10 s (the trap kills the
# hub) instead of hanging it.
refuse() {
  local file="$etc/bad-$RANDOM.toml" out i rc=0
  printf '%b' "$2" > "$file"
  "$bin" serve -config "$(native "$file")" -addr "127.0.0.1:$port_flag" > "$run_dir/refusal.txt" 2>&1 &
  hub_pid=$!
  for i in $(seq 1 100); do kill -0 "$hub_pid" 2>/dev/null || break; sleep 0.1; done
  kill -0 "$hub_pid" 2>/dev/null && fail "$1: serve accepted the file and kept running"
  wait "$hub_pid" || rc=$?
  hub_pid=""
  out="$(cat "$run_dir/refusal.txt")"
  printf '  %-26s exit %s: %s\n' "$1" "$rc" "$(printf '%s' "$out" | tail -n 1 | cut -c1-150 | sed "s|$(native "$etc")/||; s|$etc/||")"
  [ "$rc" != 0 ] || fail "$1: serve accepted the file"
  case "$out" in *DEMO_VALUE*) fail "$1: the message carries a value" ;; esac
  case "$out" in *"$(basename "$file")"*) ;; *) fail "$1: the message does not name the file" ;; esac
  cp "$run_dir/refusal.txt" "$run_dir/last-refusal.txt"
}
refuse "unknown key" 'adress = "DEMO_VALUE"\n'
grep -q 'unknown key "adress"' "$run_dir/last-refusal.txt" || fail "the unknown key is not named"
refuse "wrong type" 'backup-keep = "DEMO_VALUE"\n'
grep -q 'key "backup-keep" is a string, want an integer' "$run_dir/last-refusal.txt" || fail "the wrong type is not named"
refuse "duplicate key" 'addr = "DEMO_VALUE"\naddr = "DEMO_VALUE"\n'
grep -q 'key "addr" is set twice' "$run_dir/last-refusal.txt" || fail "the duplicate is not named"
refuse "unquoted secret" 'admin-token = DEMO_VALUE\n'
grep -q 'line 1 is not valid TOML$' "$run_dir/last-refusal.txt" || fail "the syntax error is not reported by line alone"
refuse "unreadable secret file" 'admin-token = "file:secrets/missing"\n'
grep -q 'admin-token (from config file' "$run_dir/last-refusal.txt" || fail "the setting is not named"
printf ' \n' > "$etc/secrets/empty"
refuse "empty secret file" 'admin-token = "file:secrets/empty"\n'
grep -Eq 'secrets.empty is empty' "$run_dir/last-refusal.txt" || fail "the empty file is not named"
printf 'line one\nline two DEMO_VALUE\n' > "$etc/secrets/two-lines"
refuse "two-line secret file" 'admin-token = "file:secrets/two-lines"\n'
grep -q 'more than one line' "$run_dir/last-refusal.txt" || fail "the extra line is not reported"
echo "  (every message above names the file; none carries the DEMO_VALUE each file held)"

step "5b. The old SQLite URI spelling, DATABASE_URL=file:<database>, is refused and never echoes the database"
# The database the first hub made holds audit rows with URLs in them, as any hub with a webhook does.
db_file="$(native "$run_dir/a.db")"
for command in serve config; do
  rc=0
  DATABASE_URL="file:$db_file" "$bin" "$command" -addr "127.0.0.1:$port_flag" > "$run_dir/old-uri.txt" 2>&1 &
  hub_pid=$!
  for i in $(seq 1 100); do kill -0 "$hub_pid" 2>/dev/null || break; sleep 0.1; done
  kill -0 "$hub_pid" 2>/dev/null && fail "$command started on a database file named as a secret"
  wait "$hub_pid" || rc=$?
  hub_pid=""
  printf '  %-6s exit %s, %s bytes of output: %s\n' "$command" "$rc" "$(wc -c < "$run_dir/old-uri.txt" | tr -d ' ')" \
    "$(cut -c1-150 "$run_dir/old-uri.txt" | sed "s|$run_dir/||")..."
  [ "$rc" != 0 ] || fail "$command accepted the database as a secret"
  [ "$(wc -c < "$run_dir/old-uri.txt")" -lt 700 ] || fail "$command printed a lot"
  grep -q "SQLite format" "$run_dir/old-uri.txt" && fail "$command echoed the database's contents"
  grep -q 'sqlite://file:' "$run_dir/old-uri.txt" || fail "$command does not say how a SQLite URI is written now"
done
rc=0; DATABASE_URL="sqlite://file:$db_file" "$bin" config > "$run_dir/old-uri.txt" 2>&1 || rc=$?
[ "$rc" = 0 ] || fail "the sqlite://file: spelling was refused"
echo "  and the spelling that works: DATABASE_URL=sqlite://file:... passes config ($(grep '^db ' "$run_dir/old-uri.txt" | sed "s|$run_dir/||"))"

step "6. A file next to the hub is never read unless named, and the vyshka client's VYSHKA_CONFIG is not the hub's"
printf 'addr = "127.0.0.1:1"\n' > "$away/hub.toml"; cp "$away/hub.toml" "$away/vyshka.toml"
printf '{"default":"home","profiles":{"home":{"url":"http://127.0.0.1:8080","token":"vya_client"}}}' > "$away/client.json"
VYSHKA_CONFIG="$(native "$away/client.json")" "$bin" config > "$run_dir/implicit.txt" 2>&1 || fail "the client's VYSHKA_CONFIG broke the hub"
sed -n '1p;/^addr /p' "$run_dir/implicit.txt"
grep -q '^addr .*127.0.0.1:8080 .*default' "$run_dir/implicit.txt" || fail "an unnamed file was read"
rm -f "$away/hub.toml" "$away/vyshka.toml" "$away/client.json"

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
VYSHKA_LOG_LEVEL=warn VYSHKA_ADMIN_TOKEN="$token_env" "$bin" config -config "$(native "$etc/show.toml")" -addr 127.0.0.1:9000 -panel=false \
  > "$run_dir/show.txt" 2>&1 || fail "config failed"
sed 's/^/  /' "$run_dir/show.txt"
for secret in "$secret_db" "$secret_inline" "$token_env"; do
  if grep -q "$secret" "$run_dir/show.txt"; then fail "the config output carries a secret"; fi
done
grep -q "$secret_db" "$etc/show.toml" || fail "the control grep cannot see the password"
grep -q "$secret_inline" "$etc/show.toml" || fail "the control grep cannot see the inline token"
echo "  none of the three secrets is in that output (control: the same grep finds the password and the inline token in show.toml)"
grep -qE '^db +postgres://<redacted>@db.example:5432/vyshka +file$' "$run_dir/show.txt" || fail "db is not shown with its credentials redacted and its host kept"
grep -qE '^admin-token +<redacted> +env VYSHKA_ADMIN_TOKEN$' "$run_dir/show.txt" || fail "the token is not redacted from the environment"
grep -qE '^log-level +warn +env VYSHKA_LOG_LEVEL$' "$run_dir/show.txt" || fail "env does not beat the file for log-level"
grep -qE '^backup-keep +3 +file$' "$run_dir/show.txt" || fail "backup-keep is not from the file"
grep -qE '^addr +127.0.0.1:9000 +flag$' "$run_dir/show.txt" || fail "addr is not from the flag"
grep -qE '^panel +false +flag$' "$run_dir/show.txt" || fail "panel is not from the flag"
DATABASE_URL="$(native "$run_dir/a.db")" "$bin" config > "$run_dir/plain-db.txt" 2>&1 || fail "config failed on a plain database path"
grep -qE "^db +$(native "$run_dir/a.db") +env DATABASE_URL\$" "$run_dir/plain-db.txt" || fail "a plain SQLite path is not shown"
echo "  a plain SQLite path is not a secret: $(grep '^db ' "$run_dir/plain-db.txt" | sed "s|$run_dir/||")"

step "8. The same command runs serve's checks: an unreadable secret, an incoherent auto-TLS combination"
rc=0; out="$("$bin" config -config "$(native "$etc/show.toml")" -admin-token file:/nonexistent/token 2>&1)" || rc=$?
printf '  exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1)"
[ "$rc" != 0 ] || fail "an unreadable secret passed the check"
printf 'auto-tls-email = "ops@example.org"\n' > "$etc/tls.toml"
rc=0; out="$("$bin" config -config "$(native "$etc/tls.toml")" 2>&1)" || rc=$?
printf '  exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1 | cut -c1-110)"
[ "$rc" != 0 ] || fail "an incoherent auto-TLS combination passed the check"

step "9. Inline secrets in a file that others can read earn one warning naming the keys (not on Windows)"
warnings() { "$bin" config -config "$1" 2>&1 >/dev/null | grep -c '^warning:' || true; }
if [ "$(go env GOOS)" = windows ]; then
  echo "  skipped: Windows has no permission bits (the warning logic is unit-tested there with the check forced on)"
else
  chmod 644 "$etc/show.toml"
  expect "warnings for a 0644 file holding two inline secrets" "$(warnings "$etc/show.toml")" 1
  "$bin" config -config "$etc/show.toml" 2>&1 >/dev/null | sed 's/^/  /'
  "$bin" config -config "$etc/show.toml" > "$run_dir/loose.txt" 2>&1 || fail "config failed"
  if grep -q -e "$secret_db" -e "$secret_inline" "$run_dir/loose.txt"; then fail "a warning carries a secret"; fi
  chmod 640 "$etc/show.toml"
  expect "warnings for a 0640 file (shared with a service group)" "$(warnings "$etc/show.toml")" 0
  chmod 600 "$etc/show.toml"
  expect "warnings for a 0600 file" "$(warnings "$etc/show.toml")" 0
  printf 'db = "%s"\n' "$run_dir/a.db" > "$etc/plain.toml"; chmod 644 "$etc/plain.toml"
  "$bin" config -config "$etc/plain.toml" > /dev/null 2>&1 || fail "config refused a file holding only a plain SQLite path"
  expect "warnings for a 0644 file with a plain SQLite path" "$(warnings "$etc/plain.toml")" 0
fi

step "10. The storage command takes db from the same file (db only; the other keys are checked, not used)"
"$bin" storage -config "$(native "$cfg")" > "$run_dir/storage.txt" 2>&1 || fail "storage failed"
head -n 2 "$run_dir/storage.txt" | sed "s|$(native "$run_dir")/||; s/^/  /"
grep -q '^sqlite database .*a.db' "$run_dir/storage.txt" || fail "storage did not report the file's database"
printf 'dbb = "x.db"\n' > "$etc/typo.toml"
rc=0; out="$("$bin" storage -config "$(native "$etc/typo.toml")" 2>&1)" || rc=$?
printf '  a typo in the shared file: exit %s: %s\n' "$rc" "$(printf '%s' "$out" | tail -n 1 | sed "s|$(native "$etc")/||")"
[ "$rc" != 0 ] || fail "storage accepted a file with an unknown key"

echo
echo "OK: the config file works as designed on running hubs"
