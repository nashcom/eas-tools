#!/usr/bin/env bash
# Round trip test of the whole toolbox, in one command.
#
#   1. eas-probe     one probe of the account: all its devices (and with -mail a test
#                   mail to itself that is deleted again). The gate: if it fails,
#                   nothing else is started
#   2. coordinator  nshtestherd: a running one (-herd or HERD_URL), or the container that
#                   this script starts (image nshtestherd:latest or NSHTESTHERD_IMAGE; version 0.9.2 or newer: its options have one dash)
#   3. eas-load      simulated users: probe job, directory lookups, (with -mail) mail
#                   to self (round trip), a short sync; then stop
#   4. checks       job results and the numbers of the worker
#
# Without -server everything runs against the mock server on this machine (demo).
# With -server it runs against a real server: your account only, small, gentle.
# Mail is only created there when you ask for it (-mail); in the demo it is on.
#
# Examples:
#   scripts/roundtrip.sh
#   scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw
#   scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw -only-probe
#
# Exit code: 0 all checks passed, 1 a check failed, 2 usage or setup problem.

set -u

usage()
{
  sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

Options:
  -server HOST|URL      real ActiveSync server (default: demo with the mock server)
  -user EMAIL           login of the account (real server)
  -password-file FILE   first line is the password (real server; or set EASPROBE_PASSWORD)
  -herd URL             use this running coordinator (nshtestherd, or HERD_URL) instead of starting one;
                         it must know the account (-user and -password-file are then not needed
                         for the load part)
  -only-probe           stop after the eas-probe step
  -mail                 create test mail (a round trip to yourself, deleted again):
                         opt-in for a real server, on by default in the demo
  -no-mail              no test mail (demo)
  -probe-workers N      probe up to N devices at the same time in step 1 (default 1: one
                         after the other; the first device always runs alone as the login pilot)
  -clients N            simulated users        (default: 1 real, 3 demo)
  -devices N            devices per user       (default: 1 real, 2 demo)
  -mail-count N         mails per device       (default: 1 real, 2 demo)
  -mail-wait-seconds N  seconds to wait for a mail to come back (default: 60 real, 20 demo)
  -ping N               heartbeat seconds of the probe's held-Ping test (default: 0 real, 5 demo)
  -profile NAME[,NAME]  device profile(s), used in turn for the devices (default: iphone)
  -insecure             do not verify the server certificate
  -ca-file FILE         extra CA certificates (PEM)
  -bin DIR              directory of the programs (default: the project, or bin/<platform>, see compile.sh)
  -verbose              show the worker log at the end
  -h, -help             this text

Environment: NSHTEST_TOKEN (coordinator token), EAS_PORT, HERD_PORT, METRICS_PORT
(mock server and coordinator ports, default 18080, 18788, 19117).
EOF
}

SERVER=""
USER_NAME=""
PWFILE=""
HERD="${HERD_URL:-}"
ONLY_PROBE=0
PROBE_WORKERS=1
MAIL=""
CLIENTS=""
DEVICES=""
MAIL_COUNT=""
MAIL_WAIT=""
PING=""
PROFILE="iphone"
INSECURE=0
CA_FILE=""
BIN=""
VERBOSE=0

while [ $# -gt 0 ]
do
  # the options have one dash; a double dash is read as the same option, so that nothing that calls the script breaks
  case "$1" in
    --*) first="${1#-}"; shift; set -- "$first" "$@" ;;
  esac
  case "$1" in
    -server)        SERVER="${2:-}"; shift 2 ;;
    -user)          USER_NAME="${2:-}"; shift 2 ;;
    -password-file) PWFILE="${2:-}"; shift 2 ;;
    -herd)          HERD="${2:-}"; shift 2 ;;
    -only-probe)    ONLY_PROBE=1; shift ;;
    -mail)          MAIL=1; shift ;;
    -no-mail)       MAIL=0; shift ;;
    -probe-workers) PROBE_WORKERS="${2:-1}"; shift 2 ;;
    -clients)       CLIENTS="${2:-}"; shift 2 ;;
    -devices)       DEVICES="${2:-}"; shift 2 ;;
    -mail-count)    MAIL_COUNT="${2:-}"; shift 2 ;;
    -mail-wait-seconds) MAIL_WAIT="${2:-}"; shift 2 ;;
    -ping)          PING="${2:-}"; shift 2 ;;
    -profile)       PROFILE="${2:-}"; shift 2 ;;
    -insecure)      INSECURE=1; shift ;;
    -ca-file)       CA_FILE="${2:-}"; shift 2 ;;
    -bin)           BIN="${2:-}"; shift 2 ;;
    -verbose)       VERBOSE=1; shift ;;
    -h|-help)       usage; exit 0 ;;
    *)               echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

DEMO=0
if [ -z "$SERVER" ]
then
  DEMO=1
fi

if [ "$DEMO" -eq 1 ]
then
  : "${MAIL:=1}" "${CLIENTS:=3}" "${DEVICES:=2}" "${MAIL_COUNT:=2}" "${MAIL_WAIT:=20}" "${PING:=5}"
else
  : "${MAIL:=0}" "${CLIENTS:=1}" "${DEVICES:=1}" "${MAIL_COUNT:=1}" "${MAIL_WAIT:=60}" "${PING:=0}"
fi

EAS_PORT="${EAS_PORT:-18080}"
HERD_PORT="${HERD_PORT:-18788}"
METRICS_PORT="${METRICS_PORT:-19117}"

HERE="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${BIN:-$HERE}"

find_bin()
{
  local name="$1" arch
  case "$(uname -m)" in
    aarch64|arm64) arch=arm64 ;;
    *)             arch=amd64 ;;
  esac
  local plat="linux-$arch"
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) plat="win-amd64" ;;
  esac
  for c in "$BIN/$name.exe" "$BIN/$name" "$BIN/bin/$plat/$name.exe" "$BIN/bin/$plat/$name"
  do
    if [ -x "$c" ] || [ -f "$c" ]
    then
      echo "$c"
      return 0
    fi
  done
  return 1
}

TMP="$(mktemp -d)"
PIDS=""
FAILED=0
RESULTS=""

cleanup()
{
  for p in $PIDS
  do
    kill "$p" 2>/dev/null
  done
  if [ -n "${HERD_CONTAINER:-}" ]
  then
    docker rm -f "$HERD_CONTAINER" > /dev/null 2>&1
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

say()  { echo "$*"; }
ok()   { echo "[ OK ]   $*"; RESULTS="$RESULTS\n[ OK ]   $*"; }
fail() { echo "[ FAIL ] $*"; RESULTS="$RESULTS\n[ FAIL ] $*"; FAILED=1; }
step() { echo; echo "== $*"; }

need()
{
  local p
  p="$(find_bin "$1")" || { echo "program '$1' not found (looked in $BIN); build it or use -bin" >&2; exit 2; }
  echo "$p"
}

EASPROBE="$(need eas-probe)" || exit 2
EASLOAD=""
MOCKSERVER=""
if [ "$ONLY_PROBE" -eq 0 ]
then
  EASLOAD="$(need eas-load)" || exit 2
fi
if [ "$DEMO" -eq 1 ]
then
  MOCKSERVER="$(need eas-mock-server)" || exit 2
fi

# ---- the account --------------------------------------------------------------
if [ "$DEMO" -eq 1 ]
then
  SERVER_URL="http://127.0.0.1:$EAS_PORT"
  USER_NAME="demo@example.com"
  PASSWORD="secret"
else
  SERVER_URL="$SERVER"
  if [ -z "$USER_NAME" ]
  then
    echo "-user is required with -server" >&2
    exit 2
  fi
  if [ -n "$PWFILE" ]
  then
    [ -r "$PWFILE" ] || { echo "cannot read $PWFILE" >&2; exit 2; }
    PASSWORD="$(head -n 1 "$PWFILE" | tr -d '\r')"
  elif [ -n "${EASPROBE_PASSWORD:-}" ]
  then
    PASSWORD="$EASPROBE_PASSWORD"
  else
    echo "no password: use -password-file or set EASPROBE_PASSWORD" >&2
    exit 2
  fi
fi
export EASPROBE_PASSWORD="$PASSWORD"

MAIL_ARGS=()
if [ "$MAIL" -eq 1 ]
then
  MAIL_ARGS=(-mail -mail-wait-seconds "$MAIL_WAIT")
fi

TLS_ARGS=()
[ "$INSECURE" -eq 1 ] && TLS_ARGS+=(-insecure)
[ -n "$CA_FILE" ] && TLS_ARGS+=(-ca-file "$CA_FILE")

port_free()
{
  if curl -s --max-time 1 "http://127.0.0.1:$1/" > /dev/null 2>&1
  then
    echo "port $1 is in use, not starting (set EAS_PORT / HERD_PORT / METRICS_PORT)" >&2
    exit 2
  fi
}

# ---- demo: the mock server comes first, the probe needs it ---------------
if [ "$DEMO" -eq 1 ]
then
  port_free "$EAS_PORT"
  step "mock ActiveSync server (eas-mock-server) on port $EAS_PORT"
  "$MOCKSERVER" -listen "127.0.0.1:$EAS_PORT" -latency 20ms -mail-every 5s > "$TMP/eas-mock-server.log" 2>&1 &
  PIDS="$PIDS $!"
  sleep 1
fi

# ---- 1. eas-probe: the gate ----------------------------------------------------
step "1. eas-probe: $USER_NAME at $SERVER_URL, $DEVICES device(s)$( [ "$MAIL" -eq 1 ] && echo ", with a test mail" )"
PROBE_OUT="$("$EASPROBE" -server "$SERVER_URL" -user "$USER_NAME" -profile "$PROFILE" -devices "$DEVICES" -workers "$PROBE_WORKERS" -ping "$PING" \
             -state-file "$TMP/probe-state.json" "${MAIL_ARGS[@]}" "${TLS_ARGS[@]}" 2>&1)"
PROBE_RC=$?
if [ "$PROBE_RC" -eq 0 ]
then
  # compact: the header and the step table; the details when something is wrong
  echo "$PROBE_OUT" | grep -E '^(eas-probe|=====|[0-9]+ of [0-9]+ devices|  (server|product|versions|policy|device) )|^  [a-z]+ +(ok|skipped)' | sed 's/^/   /'
  ok "eas-probe: all steps ok"
else
  echo "$PROBE_OUT" | sed 's/^/   /'
  fail "eas-probe failed (exit $PROBE_RC): nothing else is started, fix this first"
  echo
  echo "[ FAIL ] round trip failed at the probe"
  exit 1
fi

if [ "$ONLY_PROBE" -eq 1 ]
then
  echo
  echo "[ OK ]   round trip: probe only, passed"
  exit 0
fi

# ---- 2. the coordinator -------------------------------------------------------
if [ -z "$HERD" ]
then
  # No coordinator given: start the real nshtestherd as a container (image nshtestherd:latest, or
  # NSHTESTHERD_IMAGE). It is removed again at the end.
  if ! command -v docker > /dev/null 2>&1
  then
    echo "no coordinator: give a running nshtestherd with -herd URL (or HERD_URL), or install Docker so that" >&2
    echo "this script can start the nshtestherd container (docker run -d -p 8788:8788 nshtestherd:latest -bind 0.0.0.0 -generate 10 -password secret)" >&2
    exit 2
  fi
  port_free "$HERD_PORT"
  HERD="http://127.0.0.1:$HERD_PORT"
  HERD_CONTAINER="eas-roundtrip-herd-$$"
  step "2. coordinator: nshtestherd container on port $HERD_PORT"
  if [ "$DEMO" -eq 1 ]
  then
    docker run -d --rm --name "$HERD_CONTAINER" -p "127.0.0.1:$HERD_PORT:8788" "${NSHTESTHERD_IMAGE:-nshtestherd:latest}" \
      -bind 0.0.0.0 -generate $((CLIENTS + 2)) -password "$PASSWORD" > /dev/null || exit 2
  else
    short="${USER_NAME%%@*}"
    # readable inside the container (the file is in a private temp directory and is removed at the end)
    ( umask 022; printf 'Round,Trip,%s,%s,%s\n' "$PASSWORD" "$short" "$USER_NAME" > "$TMP/account.csv" )
    docker run -d --rm --name "$HERD_CONTAINER" -p "127.0.0.1:$HERD_PORT:8788" -v "$TMP/account.csv:/users.csv:ro" \
      "${NSHTESTHERD_IMAGE:-nshtestherd:latest}" -bind 0.0.0.0 -csv /users.csv > /dev/null || exit 2
    if [ "$CLIENTS" -gt 1 ]
    then
      echo "   note: the coordinator has one account (yours): -clients $CLIENTS becomes 1" >&2
      CLIENTS=1
    fi
  fi
  for i in $(seq 1 50)
  do
    curl -s --max-time 1 "$HERD/health" > /dev/null 2>&1 && break
    sleep 0.2
  done
else
  step "2. coordinator: $HERD (existing)"
fi

AUTH=()
if [ -n "${NSHTEST_TOKEN:-}" ]
then
  AUTH=(-H "Authorization: Bearer $NSHTEST_TOKEN")
fi

herd_status()
{
  curl -s "${AUTH[@]}" -H 'Accept: application/json' "$HERD/status" | tr -d ' \r\n'
}

jstat()  # jstat <key>   number from the coordinator's status
{
  herd_status | sed -n "s/.*\"$1\":\([0-9]*\).*/\1/p"
}

wait_until()  # wait_until <seconds> <description> <command...>
{
  local limit="$1" what="$2" i
  shift 2
  for i in $(seq 1 $((limit * 5)))
  do
    if "$@" > /dev/null 2>&1
    then
      return 0
    fi
    sleep 0.2
  done
  fail "timed out waiting for $what"
  return 1
}

idle_clients()
{
  [ "$(jstat clients_idle)" = "$CLIENTS" ] || [ "${CLIENTS_EXPECT_ALL:-0}" = "1" ]
}

if ! curl -s "${AUTH[@]}" "$HERD/health" > /dev/null 2>&1
then
  fail "coordinator $HERD does not answer"
  exit 1
fi
ok "coordinator answers"

# ---- 3. the worker ------------------------------------------------------------
port_free "$METRICS_PORT"
step "3. eas-load: $CLIENTS user(s) x $DEVICES device(s), profile $PROFILE"
"$EASLOAD" -herd "$HERD" -server "$SERVER_URL" -clients "$CLIENTS" -devices "$DEVICES" -profile "$PROFILE" \
  -name "roundtrip" -poll 300ms -ramp 50ms -report 0 -metrics "127.0.0.1:$METRICS_PORT" \
  -state-file "$TMP/load-state.json" "${TLS_ARGS[@]}" > "$TMP/easload.log" 2>&1 &
LOAD_PID=$!
PIDS="$PIDS $LOAD_PID"

if wait_until 40 "all $CLIENTS client(s) to register and be idle" idle_clients
then
  ok "$CLIENTS client(s) registered, idle"
else
  tail -n 15 "$TMP/easload.log"
  exit 1
fi

run_job()  # run_job <job> <params> <timeout-seconds> <description>
{
  local job="$1" params="$2" limit="$3" what="$4" ok0 failed0 i okn failedn
  ok0="$(jstat jobs_ok)"; failed0="$(jstat jobs_failed)"
  # the coordinator rejects an empty params field: leave it out
  if [ -n "$params" ]
  then
    curl -s "${AUTH[@]}" -X POST -d "target=all&command=run&job=$job" --data-urlencode "params=$params" "$HERD/command" > /dev/null
  else
    curl -s "${AUTH[@]}" -X POST -d "target=all&command=run&job=$job" "$HERD/command" > /dev/null
  fi
  for i in $(seq 1 $((limit * 5)))
  do
    okn="$(jstat jobs_ok)"; failedn="$(jstat jobs_failed)"
    if [ "${failedn:-0}" -gt "${failed0:-0}" ]
    then
      fail "$what: the job failed (see the worker log below)"
      tail -n 8 "$TMP/easload.log" | sed 's/^/        /'
      return 1
    fi
    if [ "$((${okn:-0} - ${ok0:-0}))" -ge "$CLIENTS" ]
    then
      ok "$what"
      return 0
    fi
    sleep 0.2
  done
  fail "$what: not finished after ${limit}s"
  return 1
}

metric()  # metric <regex with one group>
{
  curl -s "http://127.0.0.1:$METRICS_PORT/metrics" | sed -n "s/^$1.*/\1/p" | head -n 1
}

# ---- 4. the jobs --------------------------------------------------------------
step "4. jobs"
run_job eas-probe "" 90 "eas-probe: login, provisioning, folders, sync on every device"
run_job eas-gal "think=0.5&duration=4" 60 "eas-gal: directory lookups of the own user"

self_found="$(metric 'eas_load_directory_self_total{result="found"} \([0-9]*\)')"
self_missing="$(metric 'eas_load_directory_self_total{result="missing"} \([0-9]*\)')"
if [ "${self_found:-0}" -ge 1 ] && [ "${self_missing:-0}" -eq 0 ]
then
  ok "own user found in the directory $self_found times"
else
  fail "own user in the directory: found '${self_found:-none}', missing '${self_missing:-none}'"
fi

if [ "$MAIL" -eq 1 ]
then
  MAIL_TIMEOUT=$((60 + MAIL_COUNT * (MAIL_WAIT + 5)))
  run_job eas-mail "to=self&think=1-2&count=$MAIL_COUNT&wait=$MAIL_WAIT&cleanup=yes" "$MAIL_TIMEOUT" \
    "eas-mail: $MAIL_COUNT mail(s) per device to self, round trip"

  sent="$(metric 'eas_load_mail_sent_total{to="self"} \([0-9]*\)')"
  arrived="$(metric 'eas_load_mail_arrival_seconds_count \([0-9]*\)')"
  lost="$(metric 'eas_load_mail_lost_total \([0-9]*\)')"
  want=$((CLIENTS * DEVICES * MAIL_COUNT))
  if [ "${sent:-0}" -ge "$want" ] && [ "${arrived:-0}" -eq "${sent:-0}" ] && [ "${lost:-1}" -eq 0 ]
  then
    avg="$(curl -s "http://127.0.0.1:$METRICS_PORT/metrics" | awk '/^eas_load_mail_arrival_seconds_sum /{s=$2} /^eas_load_mail_arrival_seconds_count /{c=$2} END{ if (c>0) printf "%.2f", s/c }')"
    ok "mail: $sent sent, $arrived arrived, none lost, average round trip ${avg}s"
  else
    fail "mail: sent '${sent:-none}' (want $want), arrived '${arrived:-none}', lost '${lost:-none}'"
  fi
else
  echo "[ -- ]   eas-mail: skipped (mail is created only with -mail)"
fi

run_job eas-sync "think=1&duration=4" 60 "eas-sync: keep the mailboxes in sync"

errors="$(curl -s "http://127.0.0.1:$METRICS_PORT/metrics" | grep -E '^eas_load_requests_total\{op="[A-Za-z]+",result="error"\}' | awk '{n+=$2} END{print n+0}')"
if [ "${errors:-0}" -eq 0 ]
then
  ok "no failed requests"
else
  fail "$errors failed request(s): see eas_load_http_status_total / the worker log"
fi
refused="$(curl -s "http://127.0.0.1:$METRICS_PORT/metrics" | sed -n 's/^eas_load_failures_total{class="\(auth\|forbidden\)"} \([0-9]*\).*/\2/p' | awk '{n+=$1} END{print n+0}')"
[ "${refused:-0}" -eq 0 ] || fail "$refused login(s) were refused: check password, lockouts, block lists"

# ---- 5. stop ------------------------------------------------------------------
step "5. stop"
curl -s "${AUTH[@]}" -X POST -d "target=all&command=stop" "$HERD/command" > /dev/null
if wait_until 30 "the worker to end" bash -c "! kill -0 $LOAD_PID 2>/dev/null"
then
  ok "worker ended after stop"
fi

if [ "$VERBOSE" -eq 1 ]
then
  echo "--- worker log ---"
  tail -n 40 "$TMP/easload.log"
fi

echo
echo "== result"
printf "%b\n" "$RESULTS" | sed '/^$/d'
if [ "$FAILED" -eq 0 ]
then
  echo "[ OK ]   round trip passed"
  exit 0
fi
echo "[ FAIL ] round trip failed (use -verbose for the worker log)"
exit 1
