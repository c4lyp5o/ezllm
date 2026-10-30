#!/usr/bin/env python3
"""Tests for bridge_update.py: python3 -m unittest scripts/bridge_update_test.py"""
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bridge_update as bu  # noqa: E402

TZ = dt.timezone(dt.timedelta(hours=8))


def at(s):
    return dt.datetime.fromisoformat(s).replace(tzinfo=TZ)


class Home(unittest.TestCase):
    """Each test gets its own HOME: every bridge path is ~-relative."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name)
        patcher = mock.patch.dict(os.environ, {"HOME": str(self.home)})
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for p in self.home.rglob("*"):
            if p.is_dir() and not p.is_symlink():
                p.chmod(0o755)
        self.tmp.cleanup()

    def root(self, name):
        r = Path(bu.BRIDGES[name]["root"]).expanduser()
        r.mkdir(parents=True, exist_ok=True)
        return r


class Window(unittest.TestCase):
    def test_plain_and_wrapping_windows(self):
        self.assertTrue(bu.in_window("02:00-05:00", at("2026-10-31T02:00")))
        self.assertFalse(bu.in_window("02:00-05:00", at("2026-10-31T05:00")))
        self.assertFalse(bu.in_window("02:00-05:00", at("2026-10-31T22:00")))
        self.assertTrue(bu.in_window("23:00-02:00", at("2026-10-31T23:30")))
        self.assertTrue(bu.in_window("23:00-02:00", at("2026-10-31T01:59")))
        self.assertFalse(bu.in_window("23:00-02:00", at("2026-10-31T02:00")))


class Candidate(unittest.TestCase):
    rel = {"version": "2.1.308", "published": "2026-10-22T03:48:37Z"}

    def pick(self, rel=None, current="2.1.306", bcfg=None, blocked=None, seen="2026-10-30T22:00:00+08:00", when="2026-10-31T03:00"):
        return bu.pick_candidate(rel or self.rel, current, bcfg or {}, blocked or {}, seen, 3, at(when))

    def test_a_dated_release_soaks_from_publication(self):
        # Published 10-22, first seen 10-30: a fresh install does not wait again.
        self.assertEqual(self.pick(when="2026-10-31T03:00"), ("2.1.308", ""))
        v, why = self.pick(when="2026-10-24T03:00", seen="2026-10-23T00:00:00+08:00")
        self.assertIsNone(v)
        self.assertIn("soaking until 2026-10-25T03:48", why)

    def test_a_dateless_channel_soaks_from_first_sighting(self):
        rel = {"version": "1.4.11", "published": None}
        self.assertIsNone(self.pick(rel=rel, current="1.4.7", when="2026-11-02T03:00")[0])
        self.assertEqual(self.pick(rel=rel, current="1.4.7", when="2026-11-03T03:00")[0], "1.4.11")

    def test_not_newer_held_and_blocked_are_refused(self):
        self.assertEqual(self.pick(current="2.1.308"), (None, "up_to_date"))
        self.assertEqual(self.pick(current="2.1.310"), (None, "up_to_date"))
        self.assertIn("held", self.pick(bcfg={"hold": ["2.1.308"]}, when="2026-11-09T03:00")[1])
        self.assertEqual(bu.soak_start({"published": None}, "2026-10-30T22:00:00+08:00"), at("2026-10-30T22:00"))
        self.assertIn("blocked: drift", self.pick(blocked={"2.1.308": "drift"}, when="2026-11-09T03:00")[1])

    def test_versions_compare_numerically(self):
        self.assertGreater(bu.vtuple("0.166.0"), bu.vtuple("0.99.9"))


def tar_bytes(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as t:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size, info.mode = len(data), 0o755
            t.addfile(info, io.BytesIO(data))
    return buf.getvalue()


class Staging(Home):
    def release(self, kind, payload, algo="sha256", digest=None):
        src = self.home / "payload.bin"
        src.write_bytes(payload)
        return {"version": "9.9.9", "kind": kind, "url": src.as_uri(), "algo": algo,
                "digest": digest or hashlib.new(algo, payload).hexdigest()}

    def assert_read_only(self, path):
        for p in [path, *path.rglob("*")]:
            self.assertFalse(p.stat().st_mode & 0o222, f"{p} is writable")

    def test_claude_binary_is_verified_and_read_only(self):
        root = self.root("claude")
        out = bu.stage("claude", self.release("binary", b"#!/bin/sh\n"), root)
        self.assertEqual((out / "claude").read_bytes(), b"#!/bin/sh\n")
        self.assertTrue(os.access(out / "claude", os.X_OK))
        self.assert_read_only(out)
        self.assertEqual([p.name for p in root.iterdir()], ["9.9.9"], "staging leftovers")

    def test_a_digest_mismatch_stages_nothing(self):
        root = self.root("claude")
        with self.assertRaisesRegex(ValueError, "sha256 mismatch"):
            bu.stage("claude", self.release("binary", b"x", digest="0" * 64), root)
        self.assertEqual(list(root.iterdir()), [])

    def test_agy_tarball_yields_the_binary_as_agy(self):
        root = self.root("agy")
        rel = self.release("agy-tarball", tar_bytes({"antigravity": b"agy!", "README": b"r"}), algo="sha512")
        out = bu.stage("agy", rel, root)
        self.assertEqual(sorted(p.name for p in out.iterdir()), ["agy"])
        self.assert_read_only(out)

    def test_codex_package_keeps_the_whole_release_folder(self):
        root = self.root("codex")
        pkg = tar_bytes({"codex-package/bin/codex": b"c", "codex-package/bin/codex-code-mode-host": b"h",
                         "codex-package/codex-resources/bwrap": b"b"})
        out = bu.stage("codex", self.release("codex-package", pkg), root)
        self.assertTrue((out / "bin" / "codex-code-mode-host").is_file())
        self.assertTrue((out / "codex-resources" / "bwrap").is_file())

    def test_codex_package_without_the_code_mode_host_is_refused(self):
        root = self.root("codex")
        with self.assertRaisesRegex(ValueError, "code-mode-host"):
            bu.stage("codex", self.release("codex-package", tar_bytes({"bin/codex": b"c"})), root)
        self.assertEqual(list(root.iterdir()), [])

    def test_an_already_staged_version_is_reused(self):
        root = self.root("agy")
        (root / "9.9.9").mkdir()
        (root / "9.9.9" / "agy").write_bytes(b"old")
        self.assertEqual(bu.stage("agy", {"version": "9.9.9"}, root), root / "9.9.9")


class Layout(Home):
    def test_swap_current_is_a_relative_link(self):
        root = self.root("agy")
        bu.swap_current(root, "1.4.7")
        bu.swap_current(root, "1.4.11")
        self.assertEqual(os.readlink(root / "current"), "1.4.11")
        self.assertEqual(bu.current_version(root), "1.4.11")

    def test_prune_keeps_the_newest_and_the_protected(self):
        root = self.root("claude")
        for v in ["2.1.300", "2.1.306", "2.1.308", "2.1.310", "2.1.312"]:
            (root / v).mkdir()
            (root / v / "claude").write_bytes(b"x")
            bu.set_writable(root / v, False)
        (root / "claude-2.1.306").write_bytes(b"legacy")
        bu.prune(root, 3, {"2.1.300"})
        self.assertEqual(sorted(p.name for p in root.iterdir()),
                         ["2.1.300", "2.1.308", "2.1.310", "2.1.312", "claude-2.1.306"])

    def test_verdict_is_the_last_json_line(self):
        self.assertEqual(bu.parse_verdict('warn\n{"ok":true,"problems":[]}\n')["ok"], True)
        with self.assertRaises(ValueError):
            bu.parse_verdict("ezllm: boom")

    def test_config_merges_per_bridge(self):
        path = self.home / "c.json"
        path.write_text(json.dumps({"soak_days": 5, "bridges": {"codex": {"hold": ["0.166.0"]}}}))
        cfg = bu.load_config(path)
        self.assertEqual((cfg["soak_days"], cfg["bridges"]["codex"]["hold"], cfg["bridges"]["codex"]["enabled"]),
                         (5, ["0.166.0"], True))
        self.assertEqual(bu.DEFAULT_CONFIG["bridges"]["codex"]["hold"], [], "defaults mutated")


class Run(Home):
    """The promotion state machine, with discovery, docker and HTTP stubbed."""

    def setUp(self):
        super().setUp()
        self.root_ = self.root("agy")
        for v in ["1.4.7", "1.4.11"]:
            (self.root_ / v).mkdir()
            (self.root_ / v / "agy").write_bytes(b"x")
        bu.swap_current(self.root_, "1.4.7")
        self.cfg = self.home / "cfg.json"
        self.cfg.write_text(json.dumps({"bridges": {"claude": {"enabled": False}, "codex": {"enabled": False}}}))
        state = self.home / ".local/state/ezllm/bridge-update.json"
        state.parent.mkdir(parents=True)
        state.write_text(json.dumps({"first_seen": {"agy": {"1.4.11": "2026-10-20T00:00:00+08:00"}}}))
        self.state = state
        info = {"Name": "/ezllm", "Config": {"Entrypoint": ["/app/ezllm"], "User": "1000:1000", "Image": "img", "Env": []},
                "Mounts": [{"Source": str(self.root_), "Destination": "/opt/agy-bridge"}]}
        for name, value in {"discover": lambda n, b: [{"version": "1.4.11", "published": None}],
                            "inspect": lambda c: info, "stage": lambda n, r, root: root / r["version"],
                            "knows_bridge_check": lambda cmd: True, "restart_and_wait": mock.Mock(return_value=True),
                            "proof_script": lambda n, c, r: (True, "no proof script or token"),
                            "now": lambda: at("2026-10-31T03:00")}.items():
            p = mock.patch.object(bu, name, value)
            p.start()
            self.addCleanup(p.stop)

    def go(self, **kw):
        args = mock.Mock(**{"config": str(self.cfg), "bridge": None, "dry_run": False, "force_window": False, **kw})
        return bu.run(args)

    def status(self):
        return json.loads((self.root_ / "update-status.json").read_text())

    def test_a_gate_refusal_blocks_the_version_and_changes_nothing(self):
        with mock.patch.object(bu, "gate", lambda *a: {"ok": False, "problems": ["live plain turn: boom"]}):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertEqual((self.status()["result"], self.status()["detail"]), ("blocked", "live plain turn: boom"))
        self.assertIn("1.4.11", json.loads(self.state.read_text())["blocked"]["agy"])
        bu.restart_and_wait.assert_not_called()

    def test_a_passing_release_is_promoted_and_recorded(self):
        with mock.patch.object(bu, "gate", lambda *a: {"ok": True}), \
             mock.patch.object(bu, "prod_check", lambda *a, **k: {"ok": True}):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.11")
        self.assertEqual((self.status()["result"], self.status()["previous"]), ("updated", "1.4.7"))
        self.assertIn("1.4.11", json.loads((self.root_ / "qualified.json").read_text())["versions"])
        bu.restart_and_wait.assert_called_once()

    def test_a_failed_proof_rolls_back(self):
        checks = iter([{"ok": False, "problems": ["live tool turn: no get_weather"]}, {"ok": True}])
        with mock.patch.object(bu, "gate", lambda *a: {"ok": True}), \
             mock.patch.object(bu, "prod_check", lambda *a, **k: next(checks)):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertEqual(self.status()["result"], "rolled_back")
        self.assertFalse((self.root_ / "qualified.json").exists())
        self.assertEqual(bu.restart_and_wait.call_count, 2)

    def test_outside_the_window_nothing_is_staged(self):
        with mock.patch.object(bu, "now", lambda: at("2026-10-31T12:00")), \
             mock.patch.object(bu, "gate", mock.Mock()) as gate:
            self.assertEqual(self.go(), 0)
        gate.assert_not_called()
        self.assertEqual(self.status()["result"], "waiting")
        self.assertIn("window", self.status()["detail"])

    def test_promotion_needs_the_migrated_mount(self):
        info = bu.inspect("ezllm")
        info["Mounts"] = [{"Source": str(self.root_ / "1.4.7" / "agy"), "Destination": "/opt/agy/agy"}]
        with mock.patch.object(bu, "gate", mock.Mock()) as gate:
            self.go()
        gate.assert_not_called()
        self.assertIn("migrate the compose mounts", self.status()["detail"])

    def test_the_newest_soaked_release_wins_over_a_newer_soaking_one(self):
        self.cfg.write_text(json.dumps({"bridges": {"claude": {"enabled": False}, "agy": {"enabled": False}}}))
        root = self.root("codex")
        (root / "0.163.0").mkdir()
        bu.swap_current(root, "0.163.0")
        info = bu.inspect("ezllm")
        info["Mounts"] = [{"Source": str(root), "Destination": "/opt/codex-bridge"}]
        rels = [{"version": "0.166.0", "published": "2026-10-29T19:47:53Z"},
                {"version": "0.165.0", "published": "2026-10-26T20:09:38Z"},
                {"version": "0.163.0", "published": "2026-10-20T00:00:00Z"}]
        gated = []
        with mock.patch.object(bu, "discover", lambda n, b: rels), \
             mock.patch.object(bu, "gate", lambda n, d, i, c: gated.append(d.name) or {"ok": False, "problems": ["x"]}):
            self.go()
        self.assertEqual(gated, ["0.165.0"])

    def test_dry_run_gates_but_never_promotes(self):
        info = bu.inspect("ezllm")
        info["Mounts"] = []
        with mock.patch.object(bu, "gate", lambda *a: {"ok": True}):
            self.go(dry_run=True)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertIn("dry run", self.status()["detail"])
        bu.restart_and_wait.assert_not_called()


if __name__ == "__main__":
    unittest.main()
