#!/bin/bash
############################################################################
# Copyright Nash!Com, Daniel Nashed 2026  - APACHE 2.0 see LICENSE
############################################################################

# Collects the licenses of the libraries that are linked into the programs, with the google go-licenses tool:
#
#   third_party/licenses/   the license and notice files of every dependency (they travel with the binaries and the images)
#   THIRD_PARTY.md          the list: dependency, license, where the license text is
#
# Run it by hand when the dependencies change (go.mod), and commit what it writes. It is not part of CI or of the image builds.
# The analysis is for Linux (GOOS=linux): that is what the images contain.
#
# Usage: scripts/licenses.sh [-check] [-h]
#   -check   only check that no dependency has a license that must not be shipped (GPL, AGPL, ...), write nothing

# go-licenses v2 knows the newest Go versions; GO_LICENSES_PKG overrides it, for example github.com/google/go-licenses@v1.6.0
GO_LICENSES_PKG=${GO_LICENSES_PKG:-github.com/google/go-licenses/v2@latest}
OWN_MODULE=github.com/nashcom/eas-tools
OUT_DIR=third_party/licenses
OUT_LIST=THIRD_PARTY.md
CHECK_ONLY=0

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
  echo "$@"
  echo

  exit 1
}

usage()
{
  echo
  echo "Usage: $(basename "$0") [options]"
  echo
  echo "Collects the licenses of the dependencies into $OUT_DIR/ and the list $OUT_LIST"
  echo
  echo
  echo "Options"
  echo "--------"
  echo
  echo "-check   only check that no dependency has a license that must not be shipped, write nothing"
  echo
  echo "GO_LICENSES_PKG=... uses another version of the tool (default $GO_LICENSES_PKG)"
  echo
}

for a in "$@"; do

  p=$(echo "$a" | awk '{print tolower($0)}')

  case "$p" in

    -check)
      CHECK_ONLY=1
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

# the repository root, whatever the directory the script is started in
cd "$(dirname "$0")/.." || log_error_exit "Cannot change to the repository root"

if [ ! -f go.mod ]; then
  log_error_exit "go.mod not found: run this script in the repository"
fi

if ! command -v go > /dev/null 2>&1; then
  log_error_exit "go is not installed"
fi

export GOOS=linux

run_tool()
{
  go run "$GO_LICENSES_PKG" "$@"
}

header "Checking the licenses of the dependencies"

if ! run_tool check ./... --ignore "$OWN_MODULE" --disallowed_types=forbidden,restricted; then
  log_error_exit "A dependency has a license that must not be shipped, or the tool failed (see above)"
fi

echo "No forbidden license."

if [ "$CHECK_ONLY" = "1" ]; then
  exit 0
fi

header "Saving the license texts to $OUT_DIR"

rm -rf "$OUT_DIR"
mkdir -p "$(dirname "$OUT_DIR")"

if ! run_tool save ./... --ignore "$OWN_MODULE" --save_path="$OUT_DIR"; then
  log_error_exit "Saving the licenses failed (see above)"
fi

header "Writing $OUT_LIST"

CSV=$(mktemp)
trap 'rm -f "$CSV"' EXIT

if ! run_tool report ./... --ignore "$OWN_MODULE" > "$CSV"; then
  log_error_exit "The report failed (see above)"
fi

{
  echo "# Third-party components"
  echo
  echo "The programs of eas-tools link the libraries below. Their license texts are in [$OUT_DIR]($OUT_DIR/), and the images carry them."
  echo "This file is made by \`scripts/licenses.sh\`: run it again when \`go.mod\` changes."
  echo

  sort -t, -k1,1 "$CSV" | awk -F, '
    { pkg[NR] = "`" $1 "`"; lic[NR] = $3; url[NR] = $2 }
    END {
      h1 = "Package"; h2 = "License"; h3 = "License text"
      w1 = length(h1); w2 = length(h2); w3 = length(h3)
      for (i = 1; i <= NR; i++) {
        if (length(pkg[i]) > w1) w1 = length(pkg[i])
        if (length(lic[i]) > w2) w2 = length(lic[i])
        if (length(url[i]) > w3) w3 = length(url[i])
      }
      printf "| %-*s | %-*s | %-*s |\n", w1, h1, w2, h2, w3, h3
      line1 = line2 = line3 = ""
      for (j = 0; j < w1; j++) line1 = line1 "-"
      for (j = 0; j < w2; j++) line2 = line2 "-"
      for (j = 0; j < w3; j++) line3 = line3 "-"
      printf "| %s | %s | %s |\n", line1, line2, line3
      for (i = 1; i <= NR; i++) {
        printf "| %-*s | %-*s | %-*s |\n", w1, pkg[i], w2, lic[i], w3, url[i]
      }
    }'
} > "$OUT_LIST"

echo "Written: $OUT_DIR/ and $OUT_LIST"
echo
echo "Check the list, then commit both."
echo
