#!/bin/bash
############################################################################
# Copyright Nash!Com, Daniel Nashed 2026  - APACHE 2.0 see LICENSE
############################################################################

CONTAINER_IMAGE=nashcom/eas-tools
CONTAINER_CMD=docker
DOCKER_FILE=container/dockerfile
IMAGE_DESCRIPTION=Alpine

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

log_error_exit()
{
  echo
  echo $@
  echo

  exit 1
}

print_runtime()
{
  hours=$((SECONDS / 3600))
  seconds=$((SECONDS % 3600))
  minutes=$((seconds / 60))
  seconds=$((seconds % 60))
  h=""; m=""; s=""
  if [ ! $hours = "1" ] ; then h="s"; fi
  if [ ! $minutes = "1" ] ; then m="s"; fi
  if [ ! $seconds = "1" ] ; then s="s"; fi
  if [ ! $hours = 0 ] ; then echo "Completed in $hours hour$h, $minutes minute$m and $seconds second$s"
  elif [ ! $minutes = 0 ] ; then echo "Completed in $minutes minute$m and $seconds second$s"
  else echo "Completed in $seconds second$s"; fi
  echo
}

usage()
{
  echo
  echo "Usage: $(basename $0) [options]"
  echo
  echo
  echo "Options"
  echo "--------"
  echo
  echo "-wolfi       build Wolfi image"
  echo "-static      build Wolfi static image (very small and stripped down without shell)"
  echo
}


for a in "$@"; do

  p=$(echo "$a" | awk '{print tolower($0)}')

  case "$p" in

    -static)
      DOCKER_FILE=container/dockerfile_static
      CONTAINER_IMAGE=$CONTAINER_IMAGE:static
      IMAGE_DESCRIPTION="Chainguard Static"
      ;;

    -wolfi)
      DOCKER_FILE=container/dockerfile_wolfi
      CONTAINER_IMAGE=$CONTAINER_IMAGE:wolfi
      IMAGE_DESCRIPTION="Chainguard Wolfi"
      ;;

    -h|/h|-\?|/\?|-help|--help|help|usage)
      usage
      exit 0
      ;;

    *)
      log_error_exit "Invalid parameter [$a]"
      ;;
  esac
done


export BUILDAH_PROGRESS=plain

header "Building eas-tools image - $IMAGE_DESCRIPTION / $CONTAINER_IMAGE"

"$CONTAINER_CMD" build --no-cache --progress=plain -f "$DOCKER_FILE" -t "$CONTAINER_IMAGE" .

echo
print_runtime
echo
