#!/usr/bin/env bash
# Release gett: bump version.txt, build every platform into dist/, commit,
# tag, push, and publish a GitHub release with notes from changes-<version>.txt.
#
#   ./build.sh          0.1.0 -> 0.1.1
#   ./build.sh minor    0.1.0 -> 0.2.0
#   ./build.sh major    0.1.0 -> 1.0.0
set -euo pipefail
cd "$(dirname "$0")"

TARGETS="windows/amd64 windows/386 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

die() { echo "build.sh: $*" >&2; exit 1; }

# --- new version -------------------------------------------------------------
OLD=$(tr -d '[:space:]' < version.txt)
IFS=. read -r MAJ MIN PAT <<< "$OLD"
case "${1:-patch}" in
  patch) PAT=$((PAT + 1)) ;;
  minor) MIN=$((MIN + 1)); PAT=0 ;;
  major) MAJ=$((MAJ + 1)); MIN=0; PAT=0 ;;
  *) die "unknown argument '$1' (use: patch, minor, major)" ;;
esac
NEW="$MAJ.$MIN.$PAT"
NOTES="changes-$NEW.txt"

# --- preflight: nothing is changed until all of this passes ------------------
[ -s "$NOTES" ] || die "write the release notes first: $NOTES"
command -v gh >/dev/null || die "gh is not installed"
gh auth status >/dev/null 2>&1 || die "run: gh auth login"
git rev-parse --verify HEAD >/dev/null 2>&1 || die "make a first commit before releasing"
DIRTY=$(git status --porcelain | grep -v " $NOTES\$" || true)
[ -z "$DIRTY" ] || die "commit or stash your changes first:"$'\n'"$DIRTY"
git rev-parse "v$NEW" >/dev/null 2>&1 && die "tag v$NEW already exists"

echo "gett $OLD -> $NEW"
go test ./... >/dev/null || die "tests failed"

# --- build -------------------------------------------------------------------
echo "$NEW" > version.txt
trap 'echo "$OLD" > version.txt; echo "build.sh: failed, version.txt restored" >&2' ERR
rm -rf dist && mkdir dist
for t in $TARGETS; do
  os=${t%/*}; arch=${t#*/}; out="dist/gett-$os-$arch"
  [ "$os" = windows ] && out="$out.exe"
  echo "  $out"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out" .
done
( cd dist && { command -v sha256sum >/dev/null && sha256sum * || shasum -a 256 *; } > SHA256SUMS )
trap - ERR

# --- commit, tag, push, release ---------------------------------------------
git add version.txt "$NOTES"
git commit -q -m "v$NEW"
git tag -a "v$NEW" -m "gett $NEW"
git push -q origin HEAD
git push -q origin "v$NEW"
gh release create "v$NEW" dist/* --title "gett $NEW" --notes-file "$NOTES"
echo "released gett $NEW"
