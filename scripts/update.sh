#!/usr/bin/env bash
# Bring the project to the latest Go release and the latest dependencies,
# then vet and test. On any failure go.mod and go.sum are restored.
#
# Usage: scripts/update.sh        (run from anywhere; it works in the project root)

set -u

cd "$(dirname "$0")/.." || exit 1

cp go.mod go.mod.bak
cp go.sum go.sum.bak

restore()
{
  mv go.mod.bak go.mod
  mv go.sum.bak go.sum
  echo "update failed: go.mod and go.sum restored" >&2
  exit 1
}

go get go@latest || restore
go get -u ./... || restore
go mod tidy || restore
go vet ./... || restore
go test -count=1 ./... || restore

rm -f go.mod.bak go.sum.bak
go version
grep -E '^(go|toolchain) ' go.mod
echo "updated; review the diff of go.mod and go.sum, rebuild the binaries"
