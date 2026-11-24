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

    def test_a_dateless_channel_soaks_from_when_an_update_became_pending(self):
        # agy shipped 1.5.0..1.5.9 about daily: each sighting restarted a
        # per-build clock, so 1.4.12 was never replaced. The clock is the
        # oldest pending sighting; versions at or below current do not count.
        seen = {"1.4.11": "2026-10-31T01:38:20+08:00", "1.4.12": "2026-10-31T06:03:17+08:00",
                "1.5.0": "2026-11-04T14:01:17+08:00", "1.5.8": "2026-11-13T13:02:48+08:00",
                "1.5.9": "2026-11-14T14:03:48+08:00"}
        since = bu.pending_since(seen, "1.5.9", "1.4.12")
        self.assertEqual(since, "2026-11-04T14:01:17+08:00")
        rel = {"version": "1.5.9", "published": None}
        self.assertEqual(self.pick(rel=rel, current="1.4.12", seen=since, when="2026-11-16T03:00"), ("1.5.9", ""))
        # Once current catches up, the next build soaks from its own sighting.
        seen["1.5.10"] = "2026-11-17T09:00:00+08:00"
        self.assertEqual(bu.pending_since(seen, "1.5.10", "1.5.9"), "2026-11-17T09:00:00+08:00")

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
        self.assert_read_only(root / "9.9.9")


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

    def test_a_dateless_channel_releasing_faster_than_the_soak_still_updates(self):
        # Production 2026-11-16: agy's manifest showed only the newest build,
        # first seen two days ago, while updates had been pending since 11-04.
        self.state.write_text(json.dumps({"first_seen": {"agy": {
            "1.4.7": "2026-10-20T00:00:00+08:00", "1.5.0": "2026-11-04T14:01:17+08:00",
            "1.5.9": "2026-11-14T14:03:48+08:00"}}}))
        gated = []
        with mock.patch.object(bu, "discover", lambda n, b: [{"version": "1.5.9", "published": None}]), \
             mock.patch.object(bu, "now", lambda: at("2026-11-16T03:00")), \
             mock.patch.object(bu, "gate", lambda n, d, i, c: gated.append(d.name) or {"ok": False, "problems": ["x"]}):
            self.go()
        self.assertEqual(gated, ["1.5.9"])

    def test_dry_run_gates_but_never_promotes(self):
        info = bu.inspect("ezllm")
        info["Mounts"] = []
        with mock.patch.object(bu, "gate", lambda *a: {"ok": True}):
            self.go(dry_run=True)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertIn("dry run", self.status()["detail"])
        bu.restart_and_wait.assert_not_called()


class Copilot(Home):
    release = Staging.release
    assert_read_only = Staging.assert_read_only

    def test_discovery_uses_only_stable_digest_verified_assets(self):
        def rel(tag, prerelease=False, digest="sha256:" + "a" * 64):
            return {"tag_name": tag, "prerelease": prerelease, "published_at": "2026-11-01T00:00:00Z",
                    "assets": [{"name": bu.COPILOT_ASSET, "digest": digest, "browser_download_url": "https://example.test/release", "size": 12}]}
        with mock.patch.object(bu, "http_json", return_value=[rel("v1.0.111"), rel("v1.0.112-0", True), rel("v1.0.109", digest=None), rel("v1.0.110")]):
            releases = bu.discover_copilot()
        self.assertEqual([r["version"] for r in releases], ["1.0.111", "1.0.110"])
        self.assertEqual(releases[0]["kind"], "copilot-tarball")

    def test_stage_keeps_the_release_directory_read_only(self):
        root = self.root("copilot")
        out = bu.stage("copilot", self.release("copilot-tarball", tar_bytes({"copilot": b"bin", "resources/required": b"resource"})), root)
        self.assertEqual((out / "copilot").read_bytes(), b"bin")
        self.assertEqual((out / "resources/required").read_bytes(), b"resource")
        self.assert_read_only(out)

    def test_init_copies_the_existing_binary_without_replacing_it(self):
        src = self.home / "bin/copilot"
        src.parent.mkdir()
        src.write_bytes(b"running")
        info = {"Config": {"Env": []}, "Mounts": [{"Source": str(src), "Destination": "/opt/copilot/copilot"}]}
        args = mock.Mock(config=str(self.home / "absent.json"), bridge="copilot")
        with mock.patch.object(bu, "inspect", return_value=info), mock.patch.object(bu, "docker", return_value=mock.Mock(stdout="GitHub Copilot CLI 1.0.105.")):
            self.assertEqual(bu.init_layout(args), 0)
        root = self.root("copilot")
        self.assertEqual(bu.current_version(root), "1.0.105")
        self.assertEqual((root / "current/copilot").read_bytes(), b"running")
        self.assertEqual(src.read_bytes(), b"running")
        self.assertFalse(src.is_symlink())


class Gate(Home):
    def info(self):
        return {"Name": "/ezllm", "Image": "sha256:running-image", "Config": {"Entrypoint": ["/app/ezllm"], "Image": "mutable:tag", "User": "1000:1000",
                "Env": ["EZLLM_CODEX_BRIDGE_CACHE=off", "EZLLM_AGY_EXPERIMENTAL_TOOLS=on", "EZLLM_COPILOT_BRIDGE_WARM_POOL=on", "EZLLM_MASTER_KEY=must-not-copy"]},
                "Mounts": [{"Source": str(self.home / "actual-login"), "Destination": "/codex-sub"}]}

    def test_gate_uses_running_image_and_production_policy_without_secrets(self):
        cfg = bu.load_config(self.home / "absent")
        verdict = {"bridge": "codex", "version": "0.169.0", "ok": True, "live": True, "policy": {"sandbox": "workspace-write", "approval_policy": "never", "history": "native-if-supported-v1", "cache": "off"}, "problems": []}
        result = mock.Mock(stdout=json.dumps(verdict), returncode=0)
        copied, requests = [], []
        def run(*args, **kwargs):
            if args[0] == "run":
                copied.append(Path(args[args.index("--env-file") + 1]).read_text())
                requests.append(args)
            return result
        with mock.patch.object(bu, "knows_bridge_check", return_value=True), mock.patch.object(bu, "docker", side_effect=run) as docker:
            self.assertTrue(bu.gate("codex", self.home / "0.169.0", self.info(), cfg)["ok"])
        args = requests[0]
        self.assertEqual(docker.call_args.args[:2], ("rm", "-f"))
        self.assertIn("sha256:running-image", args)
        self.assertNotIn("mutable:tag", args)
        self.assertIn("EZLLM_CODEX_BRIDGE_CACHE=off", copied[0])
        self.assertNotIn("EZLLM_AGY_EXPERIMENTAL_TOOLS", copied[0])
        self.assertNotIn("must-not-copy", copied[0])
        self.assertIn(str(self.home / "actual-login") + ":/codex-sub", args)
        self.assertFalse(Path(args[args.index("--env-file") + 1]).exists())
        self.assertNotIn("must-not-copy", " ".join(args))

    def test_static_wrong_version_and_ignored_policy_cannot_qualify(self):
        clean = {"bridge": "agy", "version": "1.5.8", "ok": True, "live": True, "policy": {"caller_tools": "structured-output"}, "problems": []}
        for changes in ({"live": False}, {"version": "1.4.12"}, {"policy": {}}, {"bridge": "codex"}, {"problems": ["not really passed"]}):
            verdict = {**clean, **changes}
            with self.assertRaises(ValueError):
                bu.checked_verdict(mock.Mock(stdout=json.dumps(verdict), returncode=0), "agy", True, "1.5.8", {"caller_tools": "structured-output"})
        with self.assertRaises(ValueError):
            bu.checked_verdict(mock.Mock(stdout=json.dumps(clean), returncode=1), "agy", True, "1.5.8")

    def test_config_rejects_unsafe_typos_before_updating(self):
        for patch in ({"keep": 1}, {"soak_days": -1}, {"retry_hours": -1}, {"bridges": {"copilot": {"enabled": "off"}}}, {"bridges": {"unknown": {"enabled": True}}}):
            path = self.home / "config.json"
            path.write_text(json.dumps(patch))
            with self.assertRaises(ValueError):
                bu.load_config(path)

    def test_timeout_removes_only_the_unique_gate_and_environment_file(self):
        info = self.info()
        cfg = bu.load_config(self.home / "absent")
        requests = []
        def docker(*args, **kw):
            requests.append(args)
            if args[0] == "run":
                self.assertEqual(Path(args[args.index("--env-file") + 1]).stat().st_mode & 0o777, 0o600)
                raise TimeoutError("simulated Docker client timeout")
            return mock.Mock(returncode=0, stdout="")
        with mock.patch.object(bu, "knows_bridge_check", return_value=True), mock.patch.object(bu, "docker", side_effect=docker):
            with self.assertRaises(TimeoutError):
                bu.gate("codex", self.home / "0.169.0", info, cfg)
        gate = requests[0]
        name = gate[gate.index("--name") + 1]
        self.assertTrue(name.startswith("ezllm-gate-codex-"))
        self.assertEqual(requests[-1], ("rm", "-f", name))
        self.assertFalse(Path(gate[gate.index("--env-file") + 1]).exists())

    def test_login_path_comes_from_actual_production_mount(self):
        info = self.info()
        info["Config"]["Env"].append("EZLLM_CODEX_HOME=/auth/codex")
        info["Mounts"] = [{"Source": str(self.home / "shared-auth"), "Destination": "/auth"}]
        self.assertEqual(bu.login_source("codex", info), self.home / "shared-auth/codex")
        self.assertEqual(bu.production_home("codex", info), "/auth/codex")
        info["Mounts"] = []
        with self.assertRaises(ValueError):
            bu.login_source("codex", info)

    def test_every_bridge_login_defaults_to_a_sub_folder(self):
        info = self.info()
        for name in bu.BRIDGES:
            self.assertEqual(bu.production_home(name, info), f"/{name}-sub")
            self.assertTrue(bu.BRIDGES[name]["login"].endswith(f"/.{name}-sub"))


class Reliability(Run):
    def test_quota_failure_defers_without_permanently_blocking_release(self):
        gate = mock.Mock(return_value={"ok": False, "retryable": True, "problems": ["live plain: session limit"]})
        with mock.patch.object(bu, "gate", gate):
            self.assertEqual(self.go(), 0)
            self.assertEqual(self.go(), 0)
        gate.assert_called_once()
        state = json.loads(self.state.read_text())
        self.assertNotIn("1.4.11", state["blocked"].get("agy", {}))
        self.assertIn("1.4.11", state["retry_after"]["agy"])
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        bu.restart_and_wait.assert_not_called()

    def test_legacy_quota_blocks_are_migrated_but_compatibility_blocks_stay(self):
        state = json.loads(self.state.read_text())
        state["blocked"] = {"agy": {"1.4.11": "live plain: You've hit your session limit", "1.4.10": "unknown flag --print"}}
        self.state.write_text(json.dumps(state))
        with mock.patch.object(bu, "gate", mock.Mock()) as gate:
            self.go()
        gate.assert_not_called()
        state = json.loads(self.state.read_text())
        self.assertNotIn("1.4.11", state["blocked"]["agy"])
        self.assertIn("1.4.10", state["blocked"]["agy"])
        self.assertIn("1.4.11", state["retry_after"]["agy"])

    def test_window_is_rechecked_after_a_long_gate(self):
        current = [at("2026-10-31T04:59")]
        def gate(*args):
            current[0] = at("2026-10-31T05:01")
            return {"ok": True}
        with mock.patch.object(bu, "now", lambda: current[0]), mock.patch.object(bu, "gate", gate):
            self.go()
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertIn("window closed", self.status()["detail"])
        bu.restart_and_wait.assert_not_called()

    def test_post_promotion_timeout_rolls_back_and_clears_journal(self):
        with mock.patch.object(bu, "gate", return_value={"ok": True}), \
             mock.patch.object(bu, "prod_check", side_effect=[TimeoutError(), {"ok": True}]):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertEqual(self.status()["result"], "rolled_back")
        state = json.loads(self.state.read_text())
        self.assertNotIn("pending", state)
        self.assertNotIn("agy", state["promoted_at"])
        self.assertIn("1.4.11", state["retry_after"]["agy"])
        bu.restart_and_wait.assert_called()

    def test_proof_exception_also_rolls_back(self):
        with mock.patch.object(bu, "gate", return_value={"ok": True}), \
             mock.patch.object(bu, "prod_check", return_value={"ok": True}), \
             mock.patch.object(bu, "proof_script", side_effect=TimeoutError()):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertEqual(bu.restart_and_wait.call_count, 2)
        self.assertNotIn("pending", json.loads(self.state.read_text()))

    def test_failed_rollback_retains_journal_for_next_run(self):
        with mock.patch.object(bu, "gate", return_value={"ok": True}), \
             mock.patch.object(bu, "restart_and_wait", return_value=False), \
             mock.patch.object(bu, "notify_fallback", mock.Mock()) as fallback:
            self.assertEqual(self.go(), 2)
        self.assertIn("pending", json.loads(self.state.read_text()))
        self.assertEqual(self.status()["result"], "error")
        fallback.assert_called_once()

    def test_interrupted_promotion_is_recovered_outside_window_without_new_gate(self):
        state = json.loads(self.state.read_text())
        state["pending"] = {"agy": {"previous": "1.4.7", "candidate": "1.4.11"}}
        self.state.write_text(json.dumps(state))
        bu.swap_current(self.root_, "1.4.11")
        with mock.patch.object(bu, "now", lambda: at("2026-10-31T12:00")), \
             mock.patch.object(bu, "gate", mock.Mock()) as gate, \
             mock.patch.object(bu, "prod_check", return_value={"ok": True}):
            self.assertEqual(self.go(), 0)
        gate.assert_not_called()
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertNotIn("pending", json.loads(self.state.read_text()))
        bu.restart_and_wait.assert_called_once()

    def test_dry_run_does_not_recover_or_restart(self):
        state = json.loads(self.state.read_text())
        state["pending"] = {"agy": {"previous": "1.4.7", "candidate": "1.4.11"}}
        self.state.write_text(json.dumps(state))
        bu.swap_current(self.root_, "1.4.11")
        self.assertEqual(self.go(dry_run=True), 2)
        self.assertEqual(bu.current_version(self.root_), "1.4.11")
        self.assertIn("pending", json.loads(self.state.read_text()))
        bu.restart_and_wait.assert_not_called()

    def test_candidate_status_does_not_regress_to_current_while_newest_soaks(self):
        rels = [{"version": "1.5.0", "published": "2026-10-30T00:00:00Z"}, {"version": "1.4.7", "published": "2026-10-01T00:00:00Z"}]
        with mock.patch.object(bu, "discover", return_value=rels):
            self.go()
        self.assertEqual(self.status()["candidate"], "1.5.0")
        self.assertIn("1.5.0 soaking", self.status()["detail"])

    def test_changed_production_container_defers_promotion(self):
        old = bu.inspect("ezllm")
        new = json.loads(json.dumps(old))
        new["Config"]["Image"] = "new-image"
        with mock.patch.object(bu, "inspect", side_effect=[old, new]), mock.patch.object(bu, "gate", return_value={"ok": True}):
            self.go()
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertIn("production changed", self.status()["detail"])
        bu.restart_and_wait.assert_not_called()

    def test_commit_write_failure_preserves_journal_until_rollback(self):
        original = bu.write_json_atomic
        def write(path, data):
            if path == self.state and data.get("promoted_at", {}).get("agy"):
                raise OSError("simulated state commit failure")
            return original(path, data)
        with mock.patch.object(bu, "gate", return_value={"ok": True}), mock.patch.object(bu, "prod_check", return_value={"ok": True}), mock.patch.object(bu, "write_json_atomic", side_effect=write):
            self.assertEqual(self.go(), 0)
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertNotIn("pending", json.loads(self.state.read_text()))
        self.assertEqual(self.status()["result"], "rolled_back")


    def two_bridges(self, verdicts):
        """agy 1.4.7 -> 1.4.11 and codex 0.167.0 -> 0.168.0, both ready. verdicts
        maps a bridge to its post-promotion result; the run moves codex, then agy."""
        self.cfg.write_text(json.dumps({"bridges": {"claude": {"enabled": False}, "copilot": {"enabled": False}}}))
        root = self.root("codex")
        for v in ("0.167.0", "0.168.0"):
            (root / v / "bin").mkdir(parents=True)
            (root / v / "bin/codex").write_bytes(b"fixture")
        bu.swap_current(root, "0.167.0")
        bu.inspect("ezllm")["Mounts"].append({"Source": str(root), "Destination": "/opt/codex-bridge"})
        self.seen = []
        def release(name, cfg):
            return [{"version": "0.168.0" if name == "codex" else "1.4.11", "published": "2026-10-20T00:00:00Z"}]
        def check(name, info, live=True, **kw):
            if live:
                self.seen.append((name, bu.current_version(root), bu.current_version(self.root_)))
                if isinstance(verdict := verdicts.get(name), Exception):
                    raise verdict
                return verdict or {"ok": True}
            return {"ok": True}
        for target, value in {"discover": release, "gate": mock.Mock(return_value={"ok": True}), "prod_check": check}.items():
            p = mock.patch.object(bu, target, value)
            p.start()
            self.addCleanup(p.stop)
        return root

    def test_one_failing_bridge_leaves_the_others_updated(self):
        for bad in ({"ok": False, "problems": ["the turn timed out"], "retryable": True},
                    {"ok": False, "problems": ["tool protocol changed"], "retryable": False}, TimeoutError()):
            for failing in ("codex", "agy"):
                with self.subTest(failing=failing, bad=str(bad)):
                    self.doCleanups()
                    self.setUp()
                    codex = self.two_bridges({failing: bad})
                    self.assertEqual(self.go(), 0)
                    self.assertEqual(bu.current_version(codex), "0.167.0" if failing == "codex" else "0.168.0")
                    self.assertEqual(bu.current_version(self.root_), "1.4.7" if failing == "agy" else "1.4.11")
                    state = json.loads(self.state.read_text())
                    good, good_root, good_v = ("agy", self.root_, "1.4.11") if failing == "codex" else ("codex", codex, "0.168.0")
                    bad_root, bad_v = (codex, "0.168.0") if failing == "codex" else (self.root_, "1.4.11")
                    self.assertNotIn("pending", state)
                    self.assertEqual(list(state["promoted_at"]), [good])
                    self.assertNotIn(good, state["retry_after"])
                    self.assertNotIn(good, state["blocked"])
                    self.assertIn(bad_v, state["retry_after"][failing])
                    compat = isinstance(bad, dict) and not bad["retryable"]
                    self.assertEqual(bad_v in state["blocked"].get(failing, {}), compat)
                    self.assertEqual(json.loads((good_root / "update-status.json").read_text())["result"], "updated")
                    self.assertEqual(json.loads((bad_root / "update-status.json").read_text())["result"], "rolled_back")
                    self.assertIn(good_v, json.loads((good_root / "qualified.json").read_text())["versions"])
                    self.assertFalse((bad_root / "qualified.json").exists())
                    # one restart for the good bridge, promote + restore for the bad one
                    self.assertEqual(bu.restart_and_wait.call_count, 3)

    def test_each_bridge_is_proven_before_the_next_one_moves(self):
        codex = self.two_bridges({})
        self.assertEqual(self.go(), 0)
        self.assertEqual(self.seen, [("codex", "0.168.0", "1.4.7"), ("agy", "0.168.0", "1.4.11")])
        self.assertEqual(sorted(json.loads(self.state.read_text())["promoted_at"]), ["agy", "codex"])
        self.assertEqual(bu.restart_and_wait.call_count, 2)

    def test_an_incomplete_rollback_stops_the_remaining_bridges(self):
        self.two_bridges({})
        with mock.patch.object(bu, "restart_and_wait", return_value=False), mock.patch.object(bu, "notify_fallback", mock.Mock()):
            self.assertEqual(self.go(), 2)
        self.assertEqual(list(json.loads(self.state.read_text())["pending"]), ["codex"])
        self.assertEqual(bu.current_version(self.root_), "1.4.7")
        self.assertIn("codex rollback is incomplete", self.status()["detail"])
        self.assertEqual(self.seen, [])

    def test_the_window_and_container_are_rechecked_before_every_bridge(self):
        for what in ("window", "container"):
            with self.subTest(what):
                self.doCleanups()
                self.setUp()
                codex = self.two_bridges({})
                old = bu.inspect("ezllm")
                moved = {**old, "Id": "replacement"}
                restarted = []
                bu.restart_and_wait.side_effect = lambda *a: restarted.append(1) or True
                clock = (lambda: at("2026-10-31T05:30") if restarted and what == "window" else at("2026-10-31T03:00"))
                look = (lambda c: moved if restarted and what == "container" else old)
                with mock.patch.object(bu, "now", clock), mock.patch.object(bu, "inspect", look):
                    self.assertEqual(self.go(), 0)
                self.assertEqual(bu.current_version(codex), "0.168.0")
                self.assertEqual(bu.current_version(self.root_), "1.4.7")
                self.assertIn("window closed" if what == "window" else "production changed", self.status()["detail"])
                self.assertEqual(bu.restart_and_wait.call_count, 1)

    def test_a_report_failure_after_commit_does_not_stop_the_next_bridge(self):
        codex = self.two_bridges({})
        with mock.patch.object(bu, "prune", side_effect=OSError("disk")):
            self.assertEqual(self.go(), 0)
        self.assertEqual((bu.current_version(codex), bu.current_version(self.root_)), ("0.168.0", "1.4.11"))
        self.assertNotIn("pending", json.loads(self.state.read_text()))

    def test_recovery_never_restarts_a_replaced_container(self):
        state = json.loads(self.state.read_text())
        state["pending"] = {"agy": {"previous": "1.4.7", "candidate": "1.4.11"}}
        state["pending_container"] = "original-container-id"
        self.state.write_text(json.dumps(state))
        bu.swap_current(self.root_, "1.4.11")
        info = bu.inspect("ezllm")
        info["Id"] = "replacement-container-id"
        with mock.patch.object(bu, "notify_fallback", mock.Mock()):
            self.assertEqual(self.go(), 2)
        self.assertEqual(bu.current_version(self.root_), "1.4.11")
        self.assertIn("pending", json.loads(self.state.read_text()))
        bu.restart_and_wait.assert_not_called()



class SessionCachePolicy(unittest.TestCase):
    def test_cache_mode_must_be_proven(self):
        info = {"Config": {"Env": ["EZLLM_COPILOT_BRIDGE_WARM_POOL=on", "EZLLM_COPILOT_BRIDGE_CACHE=on"]}}
        self.assertIn("EZLLM_COPILOT_BRIDGE_CACHE", bu.GATE_ENV)
        self.assertEqual(bu.gate_policy("copilot", info), {"warm_pool": "on", "session_cache": "completed-prefix"})
        info["Config"]["Env"][-1] = "EZLLM_COPILOT_BRIDGE_CACHE=off"
        self.assertEqual(bu.gate_policy("copilot", info), {"warm_pool": "on"})

    def test_copilot_caches_default_on(self):
        self.assertEqual(bu.gate_policy("copilot", {"Config": {"Env": []}}), {"warm_pool": "on", "session_cache": "completed-prefix"})
        info = {"Config": {"Env": ["EZLLM_COPILOT_BRIDGE_WARM_POOL=off"]}}
        self.assertEqual(bu.gate_policy("copilot", info), {"warm_pool": "off"})


class CodexForkCachePolicy(unittest.TestCase):
    def test_fork_mode_must_be_proven(self):
        info = {"Config": {"Env": ["EZLLM_CODEX_BRIDGE_CACHE=on"]}}
        self.assertIn("EZLLM_CODEX_BRIDGE_CACHE", bu.GATE_ENV)
        self.assertEqual(bu.gate_policy("codex", info), {"sandbox": "workspace-write", "approval_policy": "never", "history": "native-if-supported-v1", "cache": "ephemeral-forks"})
        info["Config"]["Env"].pop()
        self.assertEqual(bu.gate_policy("codex", info), {"sandbox": "workspace-write", "approval_policy": "never", "history": "native-if-supported-v1", "cache": "default"})
        info["Config"]["Env"].append("EZLLM_CODEX_BRIDGE_CACHE=off")
        self.assertEqual(bu.gate_policy("codex", info), {"sandbox": "workspace-write", "approval_policy": "never", "history": "native-if-supported-v1", "cache": "off"})
        for removed in ("typo", "auto"):
            info["Config"]["Env"][-1] = "EZLLM_CODEX_BRIDGE_CACHE=" + removed
            with self.assertRaises(ValueError):
                bu.gate_policy("codex", info)
        info["Config"]["Env"][-1] = "EZLLM_CODEX_BRIDGE_CACHE=typo"
        with self.assertRaises(ValueError):
            bu.gate_policy("codex", info)

    def test_old_qualification_cannot_prove_auto_or_noninteractive_policy(self):
        for policy in ({"sandbox": "read-only"}, {"sandbox": "read-only", "cache": "auto"}, {"sandbox": "read-only", "cache": "auto", "approval_policy": "never"}):
            verdict = {"bridge": "codex", "version": "0.167.0", "ok": True, "live": True, "policy": policy, "problems": []}
            with self.assertRaises(ValueError):
                bu.checked_verdict(mock.Mock(stdout=json.dumps(verdict), returncode=0), "codex", True, "0.167.0", bu.gate_policy("codex", {"Config": {"Env": []}}))


class AGYConversationCachePolicy(unittest.TestCase):
    def test_resume_mode_must_be_proven(self):
        info = {"Config": {"Env": ["EZLLM_AGY_BRIDGE_CACHE=on"]}}
        self.assertIn("EZLLM_AGY_BRIDGE_CACHE", bu.GATE_ENV)
        self.assertEqual(bu.gate_policy("agy", info), {"caller_tools": "structured-output", "session_cache": "conversation-resume"})
        info["Config"]["Env"][-1] = "EZLLM_AGY_BRIDGE_CACHE=off"
        self.assertEqual(bu.gate_policy("agy", info), {"caller_tools": "structured-output"})

    def test_default_policy_requires_live_tools_and_resume(self):
        info = {"Config": {"Env": []}}
        self.assertEqual(bu.gate_policy("agy", info), {"caller_tools": "structured-output", "session_cache": "conversation-resume"})
        expected = bu.gate_policy("agy", info)
        verdict = {"bridge": "agy", "version": "1.5.15", "ok": True, "live": True,
                   "policy": expected, "problems": []}
        result = mock.Mock(stdout=json.dumps(verdict), returncode=0)
        self.assertTrue(bu.checked_verdict(result, "agy", True, "1.5.15", expected)["ok"])
        for incomplete in ({"caller_tools": "off"}, {"caller_tools": "structured-output"}):
            verdict["policy"] = incomplete
            result.stdout = json.dumps(verdict)
            with self.assertRaisesRegex(ValueError, "configured bridge policy"):
                bu.checked_verdict(result, "agy", True, "1.5.15", expected)

if __name__ == "__main__":
    unittest.main()
