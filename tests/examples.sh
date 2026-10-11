#!/usr/bin/env bash
# Checks the example files in deploy/ with the real programs: every JSON file and every env file of eas-probe and
# eas-load has to be accepted without a warning or an error (unknown keys and variables, wrong values), and the
# examples must not contain data of a real lab. The exporter examples are checked by the unit tests of the exporter.
# Usage: tests/examples.sh [bin-dir]   (bin-dir holds eas-probe and eas-load, with .exe on Windows)

set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-$ROOT}"
EXT=""
if [ -f "$BIN/eas-load.exe" ]
then
  EXT=".exe"
fi

FAILED=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail()
{
  echo "[ FAIL ] $*"
  FAILED=1
}

ok()
{
  echo "[ OK ]   $*"
}

# check_json <program> <file>: the program reads the file as its -config and prints its version
check_json()
{
  local prog="$1" file="$2"

  if "$BIN/$prog$EXT" -config "$ROOT/deploy/$file" -version > "$TMP/out" 2> "$TMP/err" && [ ! -s "$TMP/err" ]
  then
    ok "$file ($prog -config)"
  else
    fail "$file ($prog -config): $(cat "$TMP/err")"
  fi
}

# check_env <program> <file>: the lines of the env file are the whole environment of the program
check_env()
{
  local prog="$1" file="$2" line
  local vars=("PATH=$PATH")

  while IFS= read -r line || [ -n "$line" ]
  do
    case "$line" in
      ""|\#*) ;;
      *) vars+=("$line") ;;
    esac
  done < "$ROOT/deploy/$file"

  if env -i "${vars[@]}" "$BIN/$prog$EXT" -version > "$TMP/out" 2> "$TMP/err" && [ ! -s "$TMP/err" ]
  then
    ok "$file ($prog, $((${#vars[@]} - 1)) variables)"
  else
    fail "$file ($prog): $(cat "$TMP/err")"
  fi
}

check_json eas-probe probe.example.json
check_json eas-load load-standalone.example.json
check_json eas-load load-coordinator.example.json
check_env eas-probe probe.example.env
check_env eas-load load.example.env

# the examples are neutral: no real host, account or password file of a lab
if grep -rniE 'dnug|nashed|nsh_|nsh@|daniel' "$ROOT/deploy" > "$TMP/personal" 2>&1
then
  fail "an example holds personal data:"
  cat "$TMP/personal"
else
  ok "the files in deploy/ hold no personal data"
fi

exit $FAILED
