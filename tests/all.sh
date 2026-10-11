#!/bin/bash
############################################################################
# Copyright Nash!Com, Daniel Nashed 2026  - APACHE 2.0 see LICENSE
############################################################################

# Runs all tests and prints one result per step. This is the script that runs
# inside the test container (see ../test.sh); it also works directly on a Linux
# or WSL machine that has Go, bash and curl.
# Nothing here talks to a real server: the tests run against the mock server
# and the nshtestherd container. Tests against a real Traveler are
# scripts/roundtrip.sh and eas-probe itself.

cd "$(dirname "$0")/.." || exit 2

RUN_FMT=yes
RUN_UNIT=yes
RUN_E2E=yes
RUN_ROUNDTRIP=no
VERBOSE=no
FAILED=0
RESULTS=""

print_delim()
{
  echo "--------------------------------------------------------------------------------"
}

header()
{
  echo
  print_delim
  echo "$1"
  print_delim
  echo
}

usage()
{
  echo
  echo "Usage: tests/all.sh [options]   (normally started with ./test.sh)"
  echo
  echo "Without options: format check, vet, unit tests and the end-to-end test."
  echo
  echo "Options"
  echo "--------"
  echo
  echo "-unit        only format check, vet and unit tests (fast)"
  echo "-e2e         only the end-to-end test of the real programs"
  echo "-roundtrip   also run the round trip script (probe, coordinator, load worker)"
  echo "-all         everything, including the round trip"
  echo "-v           show the output of the tests"
  echo
}

for a in "$@"; do

  p=$(echo "$a" | awk '{print tolower($0)}')

  case "$p" in

    -unit)
      RUN_E2E=no
      ;;

    -e2e)
      RUN_FMT=no
      RUN_UNIT=no
      ;;

    -roundtrip)
      RUN_ROUNDTRIP=yes
      ;;

    -all)
      RUN_ROUNDTRIP=yes
      ;;

    -v)
      VERBOSE=yes
      ;;

    -h|-help|--help|help|usage)
      usage
      exit 0
      ;;

    *)
      echo
      echo "Invalid parameter [$a]"
      usage
      exit 2
      ;;
  esac
done

if ! command -v go > /dev/null 2>&1
then
  echo "go is not installed or not in the PATH" >&2
  exit 2
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

EXE="$(go env GOEXE)"
BINDIR="$TMP/bin"
PROGRAMS="eas-probe eas-blackbox-exporter eas-load eas-mock-server"

# run_step <name> <function>: runs the function, records ok or FAIL with the time
run_step()
{
  local name="$1" start=$SECONDS

  header "$name"

  if "$2"
  then
    RESULTS="$RESULTS\n[ OK ]   $name ($((SECONDS - start)) s)"
  else
    RESULTS="$RESULTS\n[ FAIL ] $name ($((SECONDS - start)) s)"
    FAILED=1
  fi
}

step_format()
{
  local files
  files="$(gofmt -l .)"

  if [ -n "$files" ]
  then
    echo "[ FAIL ] not formatted (run gofmt -w):"
    echo "$files"
    return 1
  fi
  echo "[ OK ]   all files formatted"
}

step_vet()
{
  go vet ./... && echo "[ OK ]   no findings"
}

step_windows()
{
  GOOS=windows GOARCH=amd64 go build ./... && echo "[ OK ]   everything compiles for Windows (not run there)"
}

step_unit()
{
  local rc

  go test -count=1 ./... > "$TMP/unit.log" 2>&1
  rc=$?

  if [ "$VERBOSE" = "yes" ] || [ "$rc" -ne 0 ]
  then
    cat "$TMP/unit.log"
  fi

  # one line per package in the standard format
  awk -F'\t' '/^ok/     { printf "[ OK ]   %-52s %s\n", $2, $3 }
              /^FAIL\t/ { printf "[ FAIL ] %-52s %s\n", $2, $3 }
              /^panic:/ { printf "[ FAIL ] %s\n", $0 }' "$TMP/unit.log"
  return $rc
}

step_build()
{
  local p

  mkdir -p "$BINDIR"
  for p in $PROGRAMS
  do
    go build -o "$BINDIR/$p$EXE" "./cmd/$p" || return 1
  done
  echo "[ OK ]   built: $PROGRAMS"
}

step_e2e()
{
  bash tests/e2e.sh "$BINDIR"
}

step_examples()
{
  bash tests/examples.sh "$BINDIR"
}

step_roundtrip()
{
  bash scripts/roundtrip.sh -bin "$BINDIR"
}


if [ "$RUN_FMT" = "yes" ]
then
  run_step "format check (gofmt)" step_format
  run_step "go vet" step_vet
fi

if [ "$RUN_UNIT" = "yes" ]
then
  run_step "unit tests (go test)" step_unit
  run_step "cross-compile for Windows" step_windows
fi

if [ "$RUN_E2E" = "yes" ] || [ "$RUN_ROUNDTRIP" = "yes" ]
then
  run_step "build the programs" step_build

  if [ "$RUN_E2E" = "yes" ] && [ "$FAILED" -eq 0 ]
  then
    run_step "example files (tests/examples.sh)" step_examples
  fi

  if [ "$RUN_E2E" = "yes" ] && [ "$FAILED" -eq 0 ]
  then
    run_step "end-to-end test (tests/e2e.sh)" step_e2e
  fi

  if [ "$RUN_ROUNDTRIP" = "yes" ] && [ "$FAILED" -eq 0 ]
  then
    run_step "round trip (scripts/roundtrip.sh)" step_roundtrip
  fi
fi

echo
print_delim
echo "Result"
print_delim
printf "$RESULTS\n"
echo

if [ "$FAILED" -eq 0 ]
then
  echo "[ OK ]   All tests passed (completed in $SECONDS seconds)"
else
  echo "[ FAIL ] Tests failed (completed in $SECONDS seconds)"
fi
echo

exit $FAILED
