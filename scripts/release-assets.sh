#!/bin/sh
# Build the standalone release binaries into release-assets/ (publish.yml, on a
# tag). Usage: scripts/release-assets.sh vX.Y.Z
set -eu
version="$1"
out=release-assets
rm -rf "$out"
mkdir -p "$out"
# CGO_ENABLED=0 for every target: static, so it runs on any distro.
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
	os="${target%/*}" arch="${target#*/}"
	name="ezllm_${version}_${os}_${arch}"
	exe=ezllm
	[ "$os" = windows ] && exe=ezllm.exe
	echo "--- $target"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/$name/$exe" ./cmd/ezllm
	cp config.example.yaml README.md "$out/$name/"
	if [ "$os" = windows ]; then
		# python3 -m zipfile: no zip(1) dependency.
		(cd "$out" && python3 -m zipfile -c "$name.zip" "$name")
	else
		tar -C "$out" -czf "$out/$name.tar.gz" "$name"
	fi
	rm -rf "${out:?}/$name"
done
(cd "$out" && sha256sum *.tar.gz *.zip > SHA256SUMS)
ls -la "$out"
