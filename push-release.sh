#!/bin/sh
# Copyright (c) 2026 Daniel Nashed / NashCom
# SPDX-License-Identifier: Apache-2.0
#
# Tags a release of eas-tools and pushes the tag: the workflow .github/workflows/release.yml then runs the tests, builds the container images
# for amd64 and arm64, pushes them to ghcr.io/nashcom/eas-tools and makes the GitHub release.
#
# The version is the constant "Version" in probe/profile.go: every program of the toolbox has it, and it is the only place that names it.
# version.txt is a convenience for anyone who wants the latest version without parsing the Go source: this script writes it and commits it.
#
# CHANGES.md must have a section "## X.Y.Z" for the version: without it nothing is tagged. The section is printed at the end, and it is the
# text of the GitHub release.
#
# Usage: ./push-release.sh [-force]
#   -force   move a tag that exists already (the images of that version are built again and replaced: not for a version that is in use)

set -eu

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
  echo "Usage: $0 [-force]" >&2
}

# The section of CHANGES.md for one version: its heading line up to the next "## " heading
changes_section()
{
  awk -v heading="## $1" '/^## / { show = ($0 == heading) } show' CHANGES.md
}

FORCE=0

case "${1:-}" in
  "")       ;;
  -force|--force) FORCE=1 ;;
  *)        usage; exit 2 ;;
esac

VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' probe/profile.go)
RELEASE="v$VERSION"

if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'
then
  echo "probe/profile.go has no version of the form: const Version = \"X.Y.Z\" (found '$VERSION'), nothing was tagged" >&2
  exit 1
fi

if [ -z "$(changes_section "$VERSION")" ]
then
  echo "CHANGES.md has no section '## $VERSION': add the changes of this release first, nothing was tagged" >&2
  exit 1
fi

# A release is made from what is committed: changes that are not committed would not be in the tag
if [ -n "$(git status --porcelain --untracked-files=no | grep -v ' version.txt$' || true)" ]
then
  echo "There are changes that are not committed: commit them first, nothing was tagged" >&2
  git status --short --untracked-files=no >&2
  exit 1
fi

if [ "$(git rev-parse --abbrev-ref HEAD)" = "HEAD" ]
then
  echo "HEAD is detached: check out a branch first, nothing was tagged" >&2
  exit 1
fi

# An image of a version is not made again by accident
EXISTS=0
if git rev-parse -q --verify "refs/tags/$RELEASE" > /dev/null || [ -n "$(git ls-remote --tags origin "refs/tags/$RELEASE")" ]
then
  EXISTS=1
fi

if [ "$EXISTS" = "1" ] && [ "$FORCE" = "0" ]
then
  echo "The tag $RELEASE exists already: raise the version in probe/profile.go and add its section to CHANGES.md, or use -force to move the tag" >&2
  exit 1
fi

header "Pushing release $RELEASE"

echo "$VERSION" > version.txt

if ! git diff --quiet -- version.txt || [ -z "$(git ls-files version.txt)" ]
then
  git add version.txt
  git commit -m "version.txt: $VERSION"
fi

# The commit that the tag points at has to be on the server
git push origin HEAD

if [ "$EXISTS" = "1" ]
then
  git tag -d "$RELEASE" 2>/dev/null || true
  git push origin ":refs/tags/$RELEASE" 2>/dev/null || true
fi

git tag -a "$RELEASE" -m "eas-tools $VERSION"
git push origin "$RELEASE"

header "Release notes for $RELEASE (from CHANGES.md; the release workflow puts them into the GitHub release)"

# Without the heading line itself: the release has its own title
changes_section "$VERSION" | sed '1d'

echo
echo "The workflow 'Release' builds the images now: https://github.com/nashcom/eas-tools/actions"
