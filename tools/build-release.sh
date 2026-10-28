#!/usr/bin/env bash
set -euo pipefail

# The archive contains one executable and its matching Chrome distribution.
target_os="${1:?Usage: build-release.sh linux|windows [output-directory]}"
case "$target_os" in
  linux) platform=linux64; binary_name=blazesolver; archive_name=blazesolver_linux_x64.tar.gz ;;
  windows) platform=win64; binary_name=blazesolver.exe; archive_name=blazesolver_windows_x64.zip ;;
  *) echo "Unsupported OS: $target_os" >&2; exit 2 ;;
esac

project_directory="$(cd "$(dirname "$0")/.." && pwd)"
output_directory="${2:-$project_directory/dist}"
mkdir -p "$output_directory"
output_directory="$(cd "$output_directory" && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf -- "$work_directory"' EXIT
package_directory="$work_directory/blazesolver"
mkdir -p "$package_directory"

catalog_url=https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json
curl --fail --silent --show-error --location --retry 3 "$catalog_url" -o "$work_directory/catalog.json"
chrome_version="$(jq -er '.channels.Stable.version' "$work_directory/catalog.json")"
chrome_url="$(jq -er --arg platform "$platform" '.channels.Stable.downloads.chrome[] | select(.platform == $platform) | .url' "$work_directory/catalog.json")"

(cd "$project_directory" && CGO_ENABLED=0 GOOS="$target_os" GOARCH=amd64   go build -trimpath -ldflags='-s -w' -o "$package_directory/$binary_name" .)
printf 'Bundling Chrome %s (%s)\n' "$chrome_version" "$platform"
curl --fail --silent --show-error --location --retry 3 "$chrome_url" -o "$work_directory/chrome.zip"
unzip -q "$work_directory/chrome.zip" -d "$work_directory"
mv "$work_directory/chrome-$platform" "$package_directory/chrome"
if [ "$target_os" = linux ]; then
  chmod +x "$package_directory/chrome/chrome" "$package_directory/chrome/chrome_crashpad_handler"
fi
cp "$project_directory/LICENSE" "$project_directory/README.md" "$package_directory/"
printf '%s\n' "$chrome_version" > "$package_directory/CHROME_VERSION"
if [ "$target_os" = windows ]; then
  (cd "$work_directory" && zip -qr "$output_directory/$archive_name" blazesolver)
else
  tar -C "$work_directory" --owner=0 --group=0 -czf "$output_directory/$archive_name" blazesolver
fi
printf 'Created %s/%s\n' "$output_directory" "$archive_name"
