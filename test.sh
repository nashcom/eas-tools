#!/bin/bash
############################################################################
# Copyright Nash!Com, Daniel Nashed 2026  - APACHE 2.0 see LICENSE
############################################################################

# Runs all tests in a Linux Go container, so the result does not depend on the
# machine you work on: ./test.sh
#
# The source is mounted read-only, nothing is written to the project. The
# downloaded Go toolchain and modules stay in a Docker volume, so only the
# first run downloads them. The mock ActiveSync server runs inside the test
# container and does not use ports of the host.
#
# The end-to-end test needs a coordinator. By default ./test.sh starts the real
# nshtestherd container next to the test container (tests/compose.yml, image
# nshtestherd:latest or NSHTESTHERD_IMAGE, for example
# ghcr.io/nashcom/nshtestherd:latest). With HERD_URL=http://host:8788 a running
# coordinator is used instead, local or remote.
#
# Options are passed on to tests/all.sh: -unit, -e2e, -roundtrip, -all, -v

CONTAINER_CMD=docker
TEST_IMAGE=nashcom/eas-tools-test
CACHE_VOLUME=eas-tools-gocache
COMPOSE_FILE=tests/compose.yml
NEED_HERD=yes

cd "$(dirname "$0")" || exit 2

for a in "$@"; do

  case "$a" in

    -unit)
      NEED_HERD=no
      ;;

    -h|-help|--help|help|usage)
      echo
      echo "Usage: $(basename $0) [options]"
      echo
      echo "Runs the format check, vet, the unit tests and the end-to-end test in a Go container."
      echo
      echo "-unit        only format check, vet and unit tests (fast, no coordinator needed)"
      echo "-e2e         only the end-to-end test of the real programs"
      echo "-roundtrip   also run the round trip script (probe, coordinator, load worker)"
      echo "-all         everything, including the round trip"
      echo "-v           show the output of the tests"
      echo
      echo "Environment"
      echo
      echo "NSHTESTHERD_IMAGE  image of the coordinator container (default nshtestherd:latest)"
      echo "HERD_URL           use this running coordinator instead of starting a container"
      echo
      exit 0
      ;;
  esac
done

if ! command -v "$CONTAINER_CMD" > /dev/null 2>&1
then
  echo "$CONTAINER_CMD is not installed or not in the PATH" >&2
  exit 2
fi

# The cache volume is shared by both ways to run the tests (a no-op when it exists)
"$CONTAINER_CMD" volume create "$CACHE_VOLUME" > /dev/null || exit 2

# The test image only adds bash and curl to the Go image; the build context is the
# small container directory, not the whole project.
"$CONTAINER_CMD" build -q -f container/dockerfile_test -t "$TEST_IMAGE" container > /dev/null || exit 2

if [ "$NEED_HERD" = "yes" ] && [ -z "${HERD_URL:-}" ]
then
  # the coordinator container starts fresh with every run (accounts are used up per run)
  trap '"$CONTAINER_CMD" compose -f "$COMPOSE_FILE" down --remove-orphans > /dev/null 2>&1' EXIT

  "$CONTAINER_CMD" compose -f "$COMPOSE_FILE" run --rm tests bash tests/all.sh "$@"
  exit $?
fi

"$CONTAINER_CMD" run --rm \
  -v "$PWD":/src:ro \
  -v "$CACHE_VOLUME":/gocache \
  -e GOMODCACHE=/gocache/mod -e GOCACHE=/gocache/build -e CGO_ENABLED=0 -e HERD_URL \
  -w /src "$TEST_IMAGE" bash tests/all.sh "$@"
exit $?
