#!/bin/sh
# Tests, then rebuilds the two click-files from src/. Needs only Docker.
#   Windows/Lutecia.exe       Windows
#   Mac/Lutecia.command       macOS (double-click) and Linux (./Lutecia.command): a short script with the macOS
#                             (Apple Silicon, Intel) and Linux programs packed after it; it starts the one for the computer.
# Builds are reproducible: unchanged source gives byte-identical files, so git sees no change.
set -eu
cd "$(dirname "$0")"
mkdir -p Windows Mac
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e CGO_ENABLED=0 \
  -v "$PWD":/repo -w /repo/src golang:1.25-alpine sh -ec '
    go vet ./... && GOOS=windows go vet ./... && GOOS=darwin go vet ./...
    go test ./...
    b() { GOOS=$1 GOARCH=$2 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$3" .; }
    t=$(mktemp -d)
    b windows amd64 ../Windows/Lutecia.exe
    b darwin  arm64 "$t/1"
    b darwin  amd64 "$t/2"
    b linux   amd64 "$t/3"

    # Header with the byte offset and size of each program. Numbers are space-padded, so the header length
    # is the same whatever they are (and never read as octal, as leading zeros would be).
    header() {
      cat <<EOF
#!/bin/sh
# Lutecia downloader for macOS (double-click it) and Linux (run ./Lutecia.command in a terminal).
# Windows: Windows/Lutecia.exe. The programs are packed below; this starts the one for this computer.
# Built by build.sh from src/; do not edit.
cd "\$(dirname "\$0")" || exit 1
case "\$(uname -s)-\$(uname -m)" in
  Darwin-arm64)  at=\$(($1)) size=\$(($2)) ;;
  Darwin-*)      at=\$(($3)) size=\$(($4)) ;;
  Linux-x86_64)  at=\$(($5)) size=\$(($6)) ;;
  *) echo "This computer (\$(uname -s) \$(uname -m)) is not supported."; read -r _; exit 1 ;;
esac
dir=\$(mktemp -d) || exit 1
tail -c +"\$at" "\$0" | head -c "\$size" > "\$dir/lutecia" && chmod +x "\$dir/lutecia" && "\$dir/lutecia" -home "\$PWD" "\$@"
code=\$?
rm -rf "\$dir"
exit \$code
EOF
    }
    pad() { printf "%12s" "$1"; }
    s1=$(wc -c < "$t/1"); s2=$(wc -c < "$t/2"); s3=$(wc -c < "$t/3")
    len=$(header 0 0 0 0 0 0 | wc -c)   # pad keeps this the final length
    h=$((len - 6 * 1 + 6 * 12))
    a1=$((h + 1)); a2=$((a1 + s1)); a3=$((a2 + s2))
    { header "$(pad $a1)" "$(pad $s1)" "$(pad $a2)" "$(pad $s2)" "$(pad $a3)" "$(pad $s3)"
      cat "$t/1" "$t/2" "$t/3"; } > ../Mac/Lutecia.command
    rm -rf "$t"'
chmod +x Windows/Lutecia.exe Mac/Lutecia.command
ls -l Windows/Lutecia.exe Mac/Lutecia.command
