"""Which release scripts/ezllm-mac.sh installs. curl is a stub, so no network and no
macOS: launchd is only touched by the other commands."""

import hashlib
import os
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("ezllm-mac.sh")

# Answers the latest-release lookup with $LATEST and serves files from $ASSETS;
# every URL asked for is written to $URLS.
CURL = """#!/bin/bash
url="${@: -1}"; out=; prev=
for a in "$@"; do [ "$prev" = -o ] && out="$a"; prev="$a"; done
echo "$url" >> "$URLS"
case "$url" in
  */releases/latest) [ -n "$LATEST" ] || exit 22; printf '%s' "$LATEST" ;;
  *) cp "$ASSETS/${url##*/}" "$out" ;;
esac
"""


class MacInstall(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        for d in ("bin", "assets", "home"):
            (self.dir / d).mkdir()
        curl = self.dir / "bin" / "curl"
        curl.write_text(CURL)
        curl.chmod(0o755)
        self.urls = self.dir / "urls"
        self.urls.touch()

    def release(self, version):
        name = f"ezllm_v{version}_darwin_arm64"
        src = self.dir / name
        src.mkdir()
        (src / "ezllm").write_text(f"binary {version}")
        (src / "config.example.yaml").write_text("listen: x\n")
        path = self.dir / "assets" / f"{name}.tar.gz"
        with tarfile.open(path, "w:gz") as t:
            t.add(src, name)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        (self.dir / "assets" / "SHA256SUMS").write_text(f"{digest}  {path.name}\n")
        return path

    def install(self, latest="", **env):
        base = {k: v for k, v in os.environ.items() if not k.startswith("EZLLM_")}
        base.update(
            PATH=f"{self.dir}/bin:{os.environ['PATH']}", HOME=str(self.dir / "home"),
            EZLLM_PREFIX=str(self.dir / "ez"), EZLLM_ARCH="arm64", LATEST=latest,
            ASSETS=str(self.dir / "assets"), URLS=str(self.urls), **env)
        return subprocess.run(["bash", str(SCRIPT), "install"], env=base, capture_output=True, text=True)

    def installed(self):
        path = self.dir / "ez" / "bin" / "ezllm"
        return path.read_text() if path.exists() else None

    def test_installs_the_latest_release(self):
        self.release("9.8.7")
        r = self.install(latest="https://github.com/c4lyp5o/ezllm/releases/tag/v9.8.7")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.installed(), "binary 9.8.7")

    def test_a_chosen_version_needs_no_lookup(self):
        self.release("1.2.3")
        for v in ("1.2.3", "v1.2.3"):
            r = self.install(EZLLM_VERSION=v)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(self.installed(), "binary 1.2.3")
        self.assertNotIn("releases/latest", self.urls.read_text())

    def test_a_failed_lookup_changes_nothing(self):
        for latest in ("", "https://github.com/c4lyp5o/ezllm/releases"):
            r = self.install(latest=latest)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("EZLLM_VERSION", r.stderr)
            self.assertFalse((self.dir / "ez").exists())

    def test_a_local_tarball_names_its_own_version(self):
        path = self.release("4.5.6")
        r = self.install(EZLLM_TARBALL=str(path))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.installed(), "binary 4.5.6")
        self.assertEqual(self.urls.read_text(), "")

    def test_only_the_owner_can_open_the_data_folder(self):
        data = self.dir / "ez" / "data"
        data.mkdir(parents=True)
        data.chmod(0o755)  # what an older script left behind
        self.release("9.8.7")
        r = self.install(EZLLM_VERSION="9.8.7")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(data.stat().st_mode & 0o777, 0o700)

    def test_a_bad_checksum_is_refused(self):
        self.release("9.8.7")
        (self.dir / "assets" / "SHA256SUMS").write_text("0" * 64 + "  ezllm_v9.8.7_darwin_arm64.tar.gz\n")
        r = self.install(EZLLM_VERSION="9.8.7")
        self.assertIn("checksum mismatch", r.stderr)
        self.assertIsNone(self.installed())


if __name__ == "__main__":
    unittest.main()
