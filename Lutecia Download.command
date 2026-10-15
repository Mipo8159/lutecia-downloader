#!/bin/sh
# macOS / Linux click-file: runs the binary for this computer from bin/, with this folder as the kit folder
# (.env next to it, data into "Bitrix import/"). Windows uses "Lutecia Download.exe" instead.
cd "$(dirname "$0")" || exit 1
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  bin=darwin-arm64 ;;
  Darwin-*)      bin=darwin-amd64 ;;
  Linux-x86_64)  bin=linux-amd64 ;;
  Linux-aarch64) bin=linux-arm64 ;;
  *) echo "This computer ($(uname -s) $(uname -m)) is not supported."; exit 1 ;;
esac
# Downloaded as a ZIP in a browser, macOS marks the binaries as quarantined; this file was approved, so clear them.
[ "$(uname -s)" = Darwin ] && xattr -dr com.apple.quarantine bin 2>/dev/null
exec "./bin/lutecia-download-$bin" -home "$PWD" "$@"
