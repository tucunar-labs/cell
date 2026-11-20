#!/usr/bin/env bash
# Builds the console and installs a desktop entry + icon for the current user.
#
#   ./install.sh            build (release) and install
#   ./install.sh --no-build install what is already in target/release
#   ./install.sh --uninstall
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
apps="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
icons="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/scalable/apps"
entry="$apps/tucunar-cell-console.desktop"
icon="$icons/tucunar-water-grid.svg"

refresh_caches() {
  command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" >/dev/null 2>&1 || true
  command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -q -t -f "${icons%/scalable/apps}" >/dev/null 2>&1 || true
  command -v kbuildsycoca6 >/dev/null 2>&1 && kbuildsycoca6 >/dev/null 2>&1 || true
}

if [ "${1:-}" = "--uninstall" ]; then
  rm -f "$entry" "$icon"
  refresh_caches
  echo "Removed $entry"
  exit 0
fi

if [ "${1:-}" != "--no-build" ]; then
  ( cd "$here" && cargo build --release --locked )
fi

chmod +x "$here/dist/tucunar-cell-console-run"
mkdir -p "$apps" "$icons"
install -m 0644 "$here/dist/tucunar-water-grid.svg" "$icon"
sed -e "s|@EXEC@|$here/dist/tucunar-cell-console-run|g" \
    -e "s|@ICON@|$icon|g" \
    "$here/dist/tucunar-cell-console.desktop.in" > "$entry"
chmod 0644 "$entry"

if command -v desktop-file-validate >/dev/null 2>&1; then
  desktop-file-validate "$entry"
fi
refresh_caches

echo "Installed $entry"
echo "Icon:      $icon"
echo "Launcher:  $here/dist/tucunar-cell-console-run"
