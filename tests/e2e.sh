#!/usr/bin/env bash
# End-to-end test of the real binaries on loopback: a mock ActiveSync server,
# a coordinator and eas-load with 3 clients, driven with curl like in the
# docs/GUIDE.md. Nothing leaves the machine. Usage: tests/e2e.sh [bin-dir]
#
# bin-dir holds eas-mock-server, eas-probe and eas-load (with .exe on Windows);
# default: the project root. The coordinator is the real nshtestherd, see HERD_URL below.

set -u

BIN="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
EAS_PORT="${EAS_PORT:-28080}"
HERD_PORT="${HERD_PORT:-28788}"
# A coordinator that is already running (for example the nshtestherd container of ./test.sh, or
# one on another machine): HERD_URL=http://host:8788. Without it the script starts the nshtestherd
# container itself (image nshtestherd:latest or NSHTESTHERD_IMAGE) and removes it again.
HERD_URL="${HERD_URL:-}"
OWN_HERD=no
if [ -z "$HERD_URL" ]
then
  OWN_HERD=yes
  HERD_URL="http://127.0.0.1:$HERD_PORT"
fi
METRICS_PORT="${METRICS_PORT:-29117}"
CLIENTS=3
DEVICES=2

EXT=""
if [ -f "$BIN/eas-mock-server.exe" ]
then
  EXT=".exe"
fi

TMP="$(mktemp -d)"
PIDS=""
FAILED=0

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

fail()
{
  echo "[ FAIL ] $*"
  FAILED=1
}

ok()
{
  echo "[ OK ]   $*"
}

# Never start on a port something already answers on (it could be someone's real service).
PORTS="$EAS_PORT $METRICS_PORT"
if [ "$OWN_HERD" = "yes" ]
then
  PORTS="$PORTS $HERD_PORT"
fi
for port in $PORTS
do
  if curl -s --max-time 1 "http://127.0.0.1:$port/" > /dev/null 2>&1
  then
    echo "port $port is in use; set EAS_PORT / HERD_PORT / METRICS_PORT" >&2
    exit 2
  fi
done

wait_for()  # wait_for <description> <command...>  (up to 20 s)
{
  local what="$1"
  shift
  local i
  for i in $(seq 1 200)
  do
    if "$@" > /dev/null 2>&1
    then
      return 0
    fi
    sleep 0.1
  done
  fail "timed out waiting for $what"
  return 1
}

herd_status()
{
  curl -s -H "Accept: application/json" "$HERD_URL/status"
}

herd_has()  # herd_has <json fragment>
{
  herd_status | tr -d ' ' | grep -q "$1"
}

metrics()
{
  curl -s "http://127.0.0.1:$METRICS_PORT/metrics"
}

command_all()  # command_all <form...>
{
  curl -s -X POST -d "target=all" "$@" "$HERD_URL/command" > /dev/null
}

"$BIN/eas-mock-server$EXT" -listen "127.0.0.1:$EAS_PORT" -latency 20ms -mail-every 1s -sim > "$TMP/eas-mock-server.log" 2>&1 &
PIDS="$PIDS $!"
if [ "$OWN_HERD" = "yes" ]
then
  if ! command -v docker > /dev/null 2>&1
  then
    echo "no coordinator: set HERD_URL to a running nshtestherd, or install Docker so that this script can start its container" >&2
    exit 2
  fi
  HERD_CONTAINER="eas-e2e-herd-$$"
  docker run -d --rm --name "$HERD_CONTAINER" -p "127.0.0.1:$HERD_PORT:8788" "${NSHTESTHERD_IMAGE:-nshtestherd:latest}" \
    -bind 0.0.0.0 -generate 5 -password secret > /dev/null || exit 2
fi

wait_for "the coordinator ($HERD_URL)" curl -sf "$HERD_URL/health" || exit 1

"$BIN/eas-load$EXT" -herd "$HERD_URL" -server "http://127.0.0.1:$EAS_PORT" \
  -clients "$CLIENTS" -devices "$DEVICES" -profile iphone,ipad -name e2e -poll 200ms -ramp 10ms -report 0 \
  -metrics "127.0.0.1:$METRICS_PORT" -state-file "$TMP/state.json" > "$TMP/easload.log" 2>&1 &
LOAD_PID=$!
PIDS="$PIDS $LOAD_PID"

if wait_for "all clients idle" herd_has "\"clients_idle\":$CLIENTS"
then
  ok "$CLIENTS clients registered and idle"
fi

# eas-probe on everybody
command_all -d command=run -d job=eas-probe
if wait_for "probe jobs ok" herd_has "\"jobs_ok\":$CLIENTS"
then
  ok "eas-probe finished ok on $CLIENTS clients"
fi

# eas-sync for a few seconds; mail arrives every second
command_all -d command=run -d job=eas-sync --data-urlencode "params=think=0.2"
if wait_for "clients running" herd_has "\"clients_running\":$CLIENTS"
then
  ok "eas-sync running on $CLIENTS clients"
fi
sleep 5

M="$(metrics)"
syncs="$(echo "$M" | sed -n 's/^eas_load_requests_total{op="Sync",result="ok"} \([0-9]*\).*/\1/p')"
added="$(echo "$M" | sed -n 's/^eas_load_items_total{kind="added"} \([0-9]*\).*/\1/p')"
errors="$(echo "$M" | sed -n 's/^eas_load_requests_total{op="Sync",result="error"} \([0-9]*\).*/\1/p')"

if [ "${syncs:-0}" -ge 30 ]
then
  ok "Sync requests counted: $syncs"
else
  fail "expected at least 30 Sync requests, got '${syncs:-none}'"
fi
if [ "${added:-0}" -ge 3 ]
then
  ok "mail items seen: $added"
else
  fail "expected new mail items from the mock server, got '${added:-none}'"
fi
if [ "${errors:-0}" -eq 0 ]
then
  ok "no Sync errors"
else
  fail "Sync errors: $errors"
fi
if echo "$M" | grep -q 'eas_load_request_duration_seconds_count{op="Sync"}'
then
  ok "latency histogram present"
else
  fail "latency histogram missing"
fi

# directory lookups: every device looks for its own user (the mock server directory
# holds every user that logged in)
command_all -d command=run -d job=eas-gal --data-urlencode "params=think=0.3"
sleep 4
M="$(metrics)"
searches="$(echo "$M" | sed -n 's/^eas_load_requests_total{op="Search",result="ok"} \([0-9]*\).*/\1/p')"
self_found="$(echo "$M" | sed -n 's/^eas_load_directory_self_total{result="found"} \([0-9]*\).*/\1/p')"
self_missing="$(echo "$M" | sed -n 's/^eas_load_directory_self_total{result="missing"} \([0-9]*\).*/\1/p')"
if [ "${searches:-0}" -ge $((CLIENTS * DEVICES)) ]
then
  ok "directory searches counted: $searches"
else
  fail "expected at least $((CLIENTS * DEVICES)) directory searches, got '${searches:-none}'"
fi
if [ "${self_found:-0}" -ge $((CLIENTS * DEVICES)) ] && [ "${self_missing:-0}" -eq 0 ]
then
  ok "own user found in the directory $self_found times, never missing"
else
  fail "own user lookups: found '${self_found:-none}', missing '${self_missing:-none}'"
fi

# mail: every device sends mail to itself and waits for it to come back (polling), 2 each
command_all -d command=run -d job=eas-mail --data-urlencode "params=to=self&think=0.2&count=2&wait=15&cleanup=yes"
sleep 8
M="$(metrics)"
mail_sent="$(echo "$M" | sed -n 's/^eas_load_mail_sent_total{to="self"} \([0-9]*\).*/\1/p')"
mail_arrived="$(echo "$M" | sed -n 's/^eas_load_mail_arrival_seconds_count \([0-9]*\).*/\1/p')"
mail_lost="$(echo "$M" | sed -n 's/^eas_load_mail_lost_total \([0-9]*\).*/\1/p')"
if [ "${mail_sent:-0}" -ge $((CLIENTS * DEVICES * 2)) ] && [ "${mail_arrived:-0}" -eq "${mail_sent:-0}" ] && [ "${mail_lost:-1}" -eq 0 ]
then
  ok "mail to self: $mail_sent sent, $mail_arrived arrived, none lost"
else
  fail "mail round trip: sent '${mail_sent:-none}', arrived '${mail_arrived:-none}', lost '${mail_lost:-none}'"
fi

# pause, then stop everybody
command_all -d command=pause -d pause_seconds=2
if wait_for "clients paused" herd_has "\"clients_paused\":$CLIENTS"
then
  ok "pause applied"
fi

command_all -d command=stop
if wait_for "eas-load to exit" bash -c "! kill -0 $LOAD_PID 2>/dev/null"
then
  ok "eas-load ended after stop"
fi

if herd_has "\"clients_done\":$CLIENTS"
then
  ok "all clients reported done"
else
  fail "clients did not all report done: $(herd_status)"
fi

if [ -s "$TMP/state.json" ] && [ "$(grep -c '"policy_key"' "$TMP/state.json")" -eq $((CLIENTS * DEVICES)) ]
then
  ok "state file holds $((CLIENTS * DEVICES)) provisioned devices ($DEVICES per account)"
else
  fail "state file missing or incomplete"
fi

# ---- negative cases: the request tells the mock server how to misbehave ------
# eas-probe sends X-Mock-Sim (-header); the mock server (-sim) acts on it. Each case
# checks the exit code (0 ok, 1 failed, 2 configuration) and the error class.

negative()  # negative <description> <expected: ok | CLASS | config> <password> <sim directive> [easprobe options...]
{
  local what="$1" want="$2" pw="$3" sim="$4" out rc
  shift 4

  out="$(EASPROBE_PASSWORD="$pw" "$BIN/eas-probe$EXT" -server "http://127.0.0.1:$EAS_PORT" -user neg@example.com \
    -no-state -directory off -step-timeout 5s -header "X-Mock-Sim: $sim" "$@" 2>&1)"
  rc=$?

  case "$want" in
    ok)
      if [ "$rc" -eq 0 ]
      then
        ok "$what: recovered"
      else
        fail "$what: expected success, exit $rc: $(echo "$out" | grep -m1 -i error)"
      fi
      ;;
    config)
      if [ "$rc" -eq 2 ]
      then
        ok "$what: refused (exit 2)"
      else
        fail "$what: expected exit 2, got $rc"
      fi
      ;;
    *)
      if [ "$rc" -eq 1 ] && echo "$out" | grep -q "\[$want\]"
      then
        ok "$what: failed with class $want"
      else
        fail "$what: expected exit 1 and class $want, got exit $rc: $(echo "$out" | grep -m1 -i error)"
      fi
      ;;
  esac
}

negative "proxy answers 502 on OPTIONS"        server_error secret "cmd=OPTIONS,http=502"
negative "server says 403"                      forbidden    secret "cmd=OPTIONS,http=403"
negative "wrong path (404)"                     not_found    secret "cmd=OPTIONS,http=404"
negative "wrong password"                       auth         wrong  "http=0"
negative "connection closed during FolderSync"  network      secret "cmd=FolderSync,close=1"
negative "Sync slower than the step timeout"    timeout      secret "cmd=Sync,delay=4s" -step-timeout 1500ms
negative "FolderSync status 6"                  eas_status   secret "cmd=FolderSync,status=6"
negative "Sync status 3 once"                   ok           secret "cmd=Sync,status=3,count=1"
negative "FolderSync status 9 once"             ok           secret "cmd=FolderSync,status=9,count=1"
negative "HTTP 409 on Sync once"                ok           secret "cmd=Sync,http=409,count=1"
negative "Authorization header is refused"      config       secret "x=1" -header "Authorization: Basic eA=="

# ---- standalone mode: eas-load without a coordinator -------------------------
# The accounts come from a CSV file (or -user), the job from the command line. The exit
# code is 0 when the job ended ok (or was stopped by -duration), 1 when it failed, 2 for
# wrong options.

printf 'FirstName,LastName,Password,Shortname,InternetAddress\nStand,One,secret,stand1,stand1@example.com\nStand,Two,secret,stand2,stand2@example.com\n' > "$TMP/users.csv"
printf 'FirstName,LastName,Password,Shortname,InternetAddress\nStand,Bad,wrong,stand3,stand3@example.com\n' > "$TMP/bad-users.csv"
printf 'secret\n' > "$TMP/pw.txt"

standalone()  # standalone <description> <expected exit code> <eas-load options...>
{
  local what="$1" want="$2" rc
  shift 2

  "$BIN/eas-load$EXT" -server "http://127.0.0.1:$EAS_PORT" -no-state -poll 200ms -ramp 10ms -report 0 "$@" > "$TMP/standalone.log" 2>&1
  rc=$?

  if [ "$rc" -eq "$want" ]
  then
    ok "$what: exit $rc"
  else
    fail "$what: expected exit $want, got $rc: $(grep -m1 -i 'error\|eas-load:' "$TMP/standalone.log")"
  fi
}

standalone "probe job on two accounts"              0 -users "$TMP/users.csv" -job eas-probe
standalone "mail job: 2 mails to self per client"   0 -users "$TMP/users.csv" -job eas-mail -params "to=self&think=0.2&count=2&wait=15"
standalone "sync job stopped by -duration"          0 -users "$TMP/users.csv" -job eas-sync -params "think=0.2" -duration 2s
standalone "one user with two devices"              0 -user stand1@example.com -password-file "$TMP/pw.txt" -devices 2 -profile iphone,ipad -job eas-probe
standalone "refused login fails the run"            1 -users "$TMP/bad-users.csv" -job eas-probe
standalone "simulated 502 fails the run"           1 -users "$TMP/users.csv" -job eas-probe -header "X-Mock-Sim: cmd=FolderSync,http=502"
standalone "-users without -job is refused"         2 -users "$TMP/users.csv"
standalone "-users and -herd exclude each other"    2 -users "$TMP/users.csv" -job eas-probe -herd http://127.0.0.1:1
standalone "-job without accounts is refused"       2 -job eas-probe

if [ "$FAILED" -ne 0 ]
then
  echo "--- eas-load.log ---"
  tail -n 30 "$TMP/easload.log"
  echo "--- eas-mock-server.log ---"
  tail -n 10 "$TMP/eas-mock-server.log"
  echo "[ FAIL ] end-to-end checks failed"
  exit 1
fi
echo "[ OK ]   all end-to-end checks passed"

