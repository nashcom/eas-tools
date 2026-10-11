#!/bin/bash
############################################################################
# Copyright Nash!Com, Daniel Nashed 2026  - APACHE 2.0 see LICENSE
############################################################################

# Builds the programs (static) in a Go container, on demand, into one folder
# per platform: bin/linux-amd64, bin/linux-arm64, bin/win-amd64
# Without options only Linux amd64 is built.

CONTAINER_CMD=docker
GO_IMAGE=golang:alpine
PROGRAMS="eas-blackbox-exporter eas-probe eas-load eas-mock-server eas-client"
PLATFORMS=""

usage()
{
  echo
  echo "Usage: $(basename $0) [options]"
  echo
  echo "Builds the programs in a Go container into bin/<platform>/ (linux-amd64, linux-arm64, win-amd64)"
  echo
  echo "Options"
  echo "--------"
  echo
  echo "-linux     Linux amd64 (default)"
  echo "-arm64     Linux arm64"
  echo "-windows   Windows amd64 (.exe files)"
  echo "-all       all of the above"
  echo
}

for a in "$@"; do

  p=$(echo "$a" | awk '{print tolower($0)}')

  case "$p" in

    -linux)
      PLATFORMS="$PLATFORMS linux/amd64"
      ;;

    -arm64)
      PLATFORMS="$PLATFORMS linux/arm64"
      ;;

    -windows)
      PLATFORMS="$PLATFORMS windows/amd64"
      ;;

    -all)
      PLATFORMS="linux/amd64 linux/arm64 windows/amd64"
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

if [ -z "$PLATFORMS" ]
then
  PLATFORMS="linux/amd64"
fi

cd "$(dirname "$0")" || exit 2

# The files are written as root in the container and handed to the calling user at the end
"$CONTAINER_CMD" run --rm -v "$PWD":/src -w /src \
  -e CGO_ENABLED=0 -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
  -e PLATFORMS="$PLATFORMS" -e PROGRAMS="$PROGRAMS" \
  "$GO_IMAGE" sh -c '
    set -e
    for p in $PLATFORMS; do
      os=${p%/*}
      arch=${p#*/}
      ext=""
      if [ "$os" = "windows" ]; then ext=".exe"; fi
      dir="$os-$arch"
      if [ "$os" = "windows" ]; then dir="win-$arch"; fi
      mkdir -p "bin/$dir"
      for c in $PROGRAMS; do
        GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "bin/$dir/$c$ext" "./cmd/$c"
      done
      echo "built bin/$dir: $PROGRAMS"
    done
    chown -R "$HOST_UID:$HOST_GID" bin
  ' || exit 1
