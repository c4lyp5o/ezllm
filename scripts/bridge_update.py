#!/usr/bin/env python3
"""Unattended bridge CLI updates for claude-bridge, codex-bridge and agy-bridge.

Design: SPEC-bridge-update.md §4.4. Runs on the host (systemd timer, hourly):

  discover  newest release per bridge on its official channel; first sightings
            are recorded so the soak can count from them (every run)
  stage     download, verify the published digest, unpack into
            <bridge-root>/<version>/, make it read-only     (window only)
  gate      `ezllm bridge-check NAME --live` in a throwaway container with the
            staged CLI and the shared login                 (window only)
  promote   swap <bridge-root>/current, restart ezllm once, then prove each
            promoted bridge in the running container; a failure swaps back
  report    <bridge-root>/update-status.json (ezllm reads it) and one log line

Stdlib only. Never prints a credential, a token or a model answer.
"""
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

UA = {"User-Agent": "ezllm-bridge-update"}
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")

DEFAULT_CONFIG = {
    "window": "02:00-05:00",
    "soak_days": 3,
    "keep": 3,
    "container": "ezllm",
    "healthz_url": "http://127.0.0.1:20129/healthz",
    "gateway_url": "http://127.0.0.1:20129",
    "proof_token_file": "~/.config/ezllm/proof.token",
    "gate_binary": "",
    "fallback_ntfy_url": "",
    "state_file": "~/.local/state/ezllm/bridge-update.json",
    "bridges": {
        "claude": {"enabled": True, "channel": "stable", "hold": []},
        "codex": {"enabled": True, "hold": []},
        "agy": {"enabled": True, "hold": []},
    },
}

# Per bridge: the host directories, where the bridge root and login appear in
# the container, and the binary's path inside a version folder.
BRIDGES = {
    "claude": {"root": "~/.local/share/claude-bridge", "login": "~/.claude-sub",
               "mount": "/opt/claude-bridge", "login_mount": "/claude-sub", "bin": "claude"},
    "codex": {"root": "~/.local/share/codex-bridge", "login": "~/.codex-sub",
              "mount": "/opt/codex-bridge", "login_mount": "/codex-sub", "bin": "bin/codex"},
    "agy": {"root": "~/.local/share/agy-bridge", "login": "~/.agy-sub",
            "mount": "/opt/agy-bridge", "login_mount": "/agy-sub", "bin": "agy"},
}

# Container environment the gate copies from production: what the bridges read
# besides their paths. Values are never logged.
GATE_ENV = ["USER", "LOGNAME", "TZ", "SSL_CERT_FILE", "EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK"]

CLAUDE_BASE = "https://downloads.claude.ai/claude-code-releases"
CODEX_RELEASES = "https://api.github.com/repos/openai/codex/releases?per_page=30"
CODEX_ASSET = "codex-package-x86_64-unknown-linux-musl.tar.gz"
AGY_MANIFEST = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_amd64.json"


def log(msg):
    print(msg, flush=True)


def vtuple(v):
    return tuple(int(p) for p in v.split("."))


def now():
    return dt.datetime.now().astimezone()


def http_bytes(url, timeout=30):
    with urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=timeout) as r:
        return r.read()


def http_json(url):
    return json.loads(http_bytes(url))


# ── discovery ────────────────────────────────────────────────────────────────
# A release is {version, published (ISO or None), url, algo, digest, kind};
# discover returns the channel's releases, newest first.

def discover_claude(channel):
    version = http_bytes(f"{CLAUDE_BASE}/{channel}").decode().strip()
    if not VERSION_RE.match(version):
        raise ValueError(f"claude {channel} channel returned no version")
    m = http_json(f"{CLAUDE_BASE}/{version}/manifest.json")
    p = m["platforms"]["linux-x64"]
    return [{"version": version, "published": m.get("buildDate"), "kind": "binary",
             "url": f"{CLAUDE_BASE}/{version}/linux-x64/{p.get('binary', 'claude')}",
             "algo": "sha256", "digest": p["checksum"], "size": p.get("size")}]


def discover_codex():
    found = []
    for rel in http_json(CODEX_RELEASES):
        tag = rel.get("tag_name", "")
        version = tag.removeprefix("rust-v")
        if rel.get("prerelease") or rel.get("draft") or not VERSION_RE.match(version):
            continue
        asset = next((a for a in rel.get("assets", []) if a.get("name") == CODEX_ASSET), None)
        if not asset or not str(asset.get("digest", "")).startswith("sha256:"):
            continue
        found.append({"version": version, "published": rel.get("published_at"), "kind": "codex-package",
                      "url": asset["browser_download_url"], "algo": "sha256",
                      "digest": asset["digest"].removeprefix("sha256:"), "size": asset.get("size")})
    if not found:
        raise ValueError("no stable codex release carries " + CODEX_ASSET)
    return sorted(found, key=lambda r: vtuple(r["version"]), reverse=True)


def discover_agy():
    m = http_json(AGY_MANIFEST)
    if not VERSION_RE.match(m.get("version", "")):
        raise ValueError("agy manifest returned no version")
    return [{"version": m["version"], "published": None, "kind": "agy-tarball",
             "url": m["url"], "algo": "sha512", "digest": m["sha512"], "size": None}]


def discover(name, bcfg):
    if name == "claude":
        return discover_claude(bcfg.get("channel", "stable"))
    if name == "codex":
        return discover_codex()
    return discover_agy()


# ── candidate selection ──────────────────────────────────────────────────────

def soak_start(release, first_seen):
    """Publication, when the channel dates its releases; otherwise (agy) when
    this updater first saw the version."""
    if release.get("published"):
        pub = dt.datetime.fromisoformat(release["published"].replace("Z", "+00:00"))
        if pub.tzinfo:
            return pub
    return dt.datetime.fromisoformat(first_seen)


def pick_candidate(release, current, bcfg, blocked, first_seen, soak_days, at):
    """(version or None, reason) — the release to try now, or why not."""
    v = release["version"]
    if current and vtuple(v) <= vtuple(current):
        return None, "up_to_date"
    if v in bcfg.get("hold", []):
        return None, f"{v} is held in the config"
    if v in blocked:
        return None, f"{v} is blocked: {blocked[v]}"
    ready = soak_start(release, first_seen) + dt.timedelta(days=soak_days)
    if at < ready:
        return None, f"{v} soaking until {ready.isoformat(timespec='minutes')}"
    return v, ""


def in_window(window, at):
    start, end = (dt.time.fromisoformat(s) for s in window.split("-"))
    t = at.time()
    return start <= t < end if start <= end else (t >= start or t < end)


# ── layout ───────────────────────────────────────────────────────────────────

def current_version(root):
    link = root / "current"
    if not link.is_symlink():
        return None
    target = os.readlink(link)
    return target if VERSION_RE.match(target) else None


def swap_current(root, version):
    """Point current at version atomically (relative link: it must resolve
    inside the container, where the root is mounted elsewhere)."""
    tmp = root / f".current-{os.getpid()}"
    if tmp.is_symlink() or tmp.exists():
        tmp.unlink()
    os.symlink(version, tmp)
    os.replace(tmp, root / "current")


def set_writable(path, writable):
    for p in [path, *path.rglob("*")]:
        if p.is_symlink():
            continue
        mode = p.stat().st_mode
        p.chmod(mode | 0o200 if writable else mode & ~0o222)


def write_json_atomic(path, data):
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix="." + path.name)
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


# ── staging ──────────────────────────────────────────────────────────────────

def download(url, dest, algo, digest):
    h = hashlib.new(algo)
    with urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=60) as r, dest.open("wb") as f:
        while chunk := r.read(1 << 20):
            h.update(chunk)
            f.write(chunk)
    if h.hexdigest() != digest.lower():
        raise ValueError(f"{algo} mismatch for {url}")


def stage(name, release, root):
    """Unpack a verified release into root/<version>, read-only. An existing
    folder with the binary is reused (staged on an earlier run)."""
    v = release["version"]
    final = root / v
    binary = final / BRIDGES[name]["bin"]
    if final.exists():
        if not binary.is_file():
            raise ValueError(f"{final} exists without {BRIDGES[name]['bin']}: remove it to restage")
        return final
    work = Path(tempfile.mkdtemp(dir=root, prefix=f".staging-{v}-"))
    try:
        payload = work / "payload"
        download(release["url"], payload, release["algo"], release["digest"])
        out = work / v
        out.mkdir()
        if release["kind"] == "binary":
            payload.rename(out / BRIDGES[name]["bin"])
        elif release["kind"] == "agy-tarball":
            with tarfile.open(payload) as t:
                member = t.getmember("antigravity")
                t.extract(member, work / "x", filter="data")
            (work / "x" / "antigravity").rename(out / "agy")
        else:
            with tarfile.open(payload) as t:
                t.extractall(work / "x", filter="data")
            # The package is the release folder (bin/codex and the code-mode
            # host the bridge needs for tools), possibly under one top folder.
            found = [p.parent.parent for p in (work / "x").glob("**/bin/codex") if len(p.relative_to(work / "x").parts) <= 3]
            if len(found) != 1 or not (found[0] / "bin" / "codex-code-mode-host").is_file():
                raise ValueError("codex package has no single release folder with bin/codex and bin/codex-code-mode-host")
            out.rmdir()
            found[0].rename(out)
        (out / BRIDGES[name]["bin"]).chmod(0o755)
        # Moving a directory rewrites its `..` entry, so it is made read-only
        # only once it is in place.
        out.rename(final)
        set_writable(final, False)
        return final
    finally:
        if work.exists():
            set_writable(work, True)
            shutil.rmtree(work)


def prune(root, keep, protect):
    versions = sorted((p for p in root.iterdir() if p.is_dir() and not p.is_symlink() and VERSION_RE.match(p.name)),
                      key=lambda p: vtuple(p.name), reverse=True)
    for p in versions[keep:]:
        if p.name in protect:
            continue
        set_writable(p, True)
        shutil.rmtree(p)
        log(f"pruned {p}")


# ── docker ───────────────────────────────────────────────────────────────────

def docker(*args, timeout=600, check=True):
    r = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout)
    if check and r.returncode != 0:
        raise RuntimeError(f"docker {args[0]} failed: {r.stderr.strip()[:300]}")
    return r


def inspect(container):
    return json.loads(docker("inspect", container).stdout)[0]


def container_env(info, keys):
    env = dict(e.split("=", 1) for e in info["Config"]["Env"] if "=" in e)
    return {k: env[k] for k in keys if k in env}


def mounts_root(info, name):
    """Production mounts the whole bridge root — the layout promotion needs."""
    root = str(Path(BRIDGES[name]["root"]).expanduser())
    return any(m.get("Source") == root and m.get("Destination") == BRIDGES[name]["mount"] for m in info["Mounts"])


def knows_bridge_check(cmd):
    """An ezllm that predates bridge-check would treat it as a stray argument
    and start a second router: ask `help` first, never the subcommand blind."""
    r = subprocess.run(cmd + ["help"], capture_output=True, text=True, timeout=60)
    return "bridge-check" in r.stdout


def parse_verdict(out):
    for line in reversed(out.strip().splitlines()):
        if line.startswith("{"):
            return json.loads(line)
    raise ValueError("bridge-check printed no verdict")


def gate(name, verdir, info, cfg):
    """bridge-check --live in a throwaway container: production's image, user
    and bridge env, the staged CLI read-only, the shared login read-write."""
    b = BRIDGES[name]
    entry = info["Config"]["Entrypoint"][0]
    args = ["run", "--rm", "--user", info["Config"]["User"] or "1000:1000",
            "-v", f"{verdir}:/opt/candidate:ro", "-v", f"{Path(b['login']).expanduser()}:{b['login_mount']}"]
    for k, v in container_env(info, GATE_ENV).items():
        args += ["-e", f"{k}={v}"]
    if cfg["gate_binary"]:
        args += ["-v", f"{Path(cfg['gate_binary']).expanduser()}:/opt/ezllm-gate:ro"]
        entry = "/opt/ezllm-gate"
    args += ["--entrypoint", entry, info["Config"]["Image"]]
    probe = ["docker", *args]
    if not knows_bridge_check(probe):
        raise RuntimeError("the gate image predates bridge-check (ezllm 1.6.0); set gate_binary or deploy 1.6.0")
    r = docker(*args, "bridge-check", name, "--bin", f"/opt/candidate/{b['bin']}",
               "--home", b["login_mount"], "--live", "--json", check=False)
    return parse_verdict(r.stdout)


def prod_check(name, info, live=True):
    """bridge-check inside the running container: its real mounts, env and
    user, through the promoted `current` path."""
    entry = info["Config"]["Entrypoint"][0]
    b = BRIDGES[name]
    args = ["exec", info["Name"].lstrip("/"), entry, "bridge-check", name, "--bin", f"{b['mount']}/current/{b['bin']}"]
    if live:
        args.append("--live")
    r = docker(*args, "--json", check=False)
    return parse_verdict(r.stdout)


def restart_and_wait(cfg, info):
    docker("restart", "-t", "30", info["Name"].lstrip("/"), timeout=120)
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            if json.loads(http_bytes(cfg["healthz_url"], timeout=5)).get("status") == "ok":
                return True
        except Exception:
            pass
        time.sleep(3)
    return False


def proof_script(name, cfg, repo):
    """The gateway-level live proof where one exists (claude, codex)."""
    script = repo / "tests" / f"prove_{name}_bridge.py"
    token_file = Path(cfg["proof_token_file"]).expanduser()
    if not script.is_file() or not token_file.is_file():
        return True, "no proof script or token"
    env = {**os.environ, "EZLLM_URL": cfg["gateway_url"], "EZLLM_TOKEN": token_file.read_text().strip()}
    r = subprocess.run([sys.executable, str(script)], env=env, capture_output=True, text=True, timeout=600)
    passed = sum(1 for l in r.stdout.splitlines() if "PASS" in l or "✓" in l)
    return r.returncode == 0, f"{script.name} exit {r.returncode} ({passed} passed)"


def notify_fallback(cfg, title, body):
    url = cfg.get("fallback_ntfy_url")
    if not url:
        return
    try:
        req = urllib.request.Request(url, data=body.encode(), headers={**UA, "Title": title, "Priority": "high"})
        urllib.request.urlopen(req, timeout=15).read()
    except Exception as e:
        log(f"fallback ntfy failed: {type(e).__name__}")


# ── run ──────────────────────────────────────────────────────────────────────

def load_config(path):
    cfg = json.loads(json.dumps(DEFAULT_CONFIG))
    if path.is_file():
        user = json.loads(path.read_text())
        for name, b in user.pop("bridges", {}).items():
            cfg["bridges"].setdefault(name, {}).update(b)
        cfg.update(user)
    return cfg


def status(root, name, **fields):
    data = {"bridge": name, "checked_at": now().isoformat(timespec="seconds"), **fields}
    write_json_atomic(root / "update-status.json", data)
    log(f"{name}: {fields.get('result')} current={fields.get('current')} "
        f"candidate={fields.get('candidate') or '-'} {fields.get('detail', '')}".rstrip())


def record_qualified(root, version, check, proof):
    path = root / "qualified.json"
    rec = json.loads(path.read_text()) if path.is_file() else {"versions": {}}
    rec.setdefault("versions", {})[version] = {"qualified_at": now().isoformat(timespec="seconds"),
                                               "check": check, "proof": proof}
    write_json_atomic(path, rec)


def run(args):
    cfg = load_config(Path(args.config).expanduser())
    state_path = Path(cfg["state_file"]).expanduser()
    state_path.parent.mkdir(parents=True, exist_ok=True)
    state = json.loads(state_path.read_text()) if state_path.is_file() else {}
    for k in ("first_seen", "blocked", "promoted_at"):
        state.setdefault(k, {})
    at = now()
    repo = Path(__file__).resolve().parent.parent
    window = args.force_window or in_window(cfg["window"], at)
    names = [args.bridge] if args.bridge else [n for n, b in cfg["bridges"].items() if b.get("enabled")]

    candidates = {}
    for name in names:
        root = Path(BRIDGES[name]["root"]).expanduser()
        cur = current_version(root)
        if cur is None:
            log(f"{name}: no {root}/current link — run with --init first")
            continue
        try:
            releases = discover(name, cfg["bridges"][name])
        except Exception as e:
            status(root, name, result="error", current=cur, detail=f"discovery failed: {e}"[:300])
            continue
        # The newest release that is ready; the reason reported is the newest's.
        rel, v, why = releases[0], None, ""
        for r in releases:
            seen = state["first_seen"].setdefault(name, {}).setdefault(r["version"], at.isoformat(timespec="seconds"))
            rv, rwhy = pick_candidate(r, cur, cfg["bridges"][name], state["blocked"].get(name, {}), seen, cfg["soak_days"], at)
            if r is releases[0]:
                why = rwhy
            if rv or rwhy == "up_to_date":
                rel, v = r, rv
                break
        last = state["promoted_at"].get(name)
        if v and last and at - dt.datetime.fromisoformat(last) < dt.timedelta(hours=12):
            v, why = None, f"promoted at {last}; one promotion per night"
        if v and not window:
            v, why = None, f"{rel['version']} ready; waiting for the {cfg['window']} window"
        if v is None:
            status(root, name, result="up_to_date" if why == "up_to_date" else "waiting", current=cur,
                   candidate=None if why == "up_to_date" else rel["version"], detail="" if why == "up_to_date" else why)
            continue
        candidates[name] = (root, cur, rel)
    write_json_atomic(state_path, state)
    if not candidates:
        return 0

    info = inspect(cfg["container"])
    passed = {}
    for name, (root, cur, rel) in candidates.items():
        v = rel["version"]
        if not args.dry_run and not mounts_root(info, name):
            status(root, name, result="error", current=cur, candidate=v,
                   detail=f"production does not mount {root} at {BRIDGES[name]['mount']}: migrate the compose mounts first")
            continue
        try:
            verdir = stage(name, rel, root)
            verdict = gate(name, verdir, info, cfg)
        except Exception as e:
            status(root, name, result="error", current=cur, candidate=v, detail=f"stage/gate: {e}"[:300])
            continue
        if not verdict.get("ok"):
            reason = "; ".join(verdict.get("problems", []))[:300]
            state["blocked"].setdefault(name, {})[v] = reason
            status(root, name, result="blocked", current=cur, candidate=v, detail=reason)
            continue
        if args.dry_run:
            status(root, name, result="waiting", current=cur, candidate=v, detail="dry run: staged and gated ok")
            continue
        passed[name] = (root, cur, v)
    write_json_atomic(state_path, state)
    if not passed:
        return 0

    if not knows_bridge_check(["docker", "exec", cfg["container"], info["Config"]["Entrypoint"][0]]):
        for name, (root, cur, v) in passed.items():
            status(root, name, result="error", current=cur, candidate=v,
                   detail="production ezllm predates bridge-check (1.6.0): cannot prove a promotion")
        return 0

    for name, (root, cur, v) in passed.items():
        swap_current(root, v)
    healthy = restart_and_wait(cfg, info)
    failed = []
    for name, (root, cur, v) in passed.items():
        verdict = prod_check(name, info) if healthy else {"ok": False, "problems": ["ezllm did not come back healthy"]}
        ok, proof = (proof_script(name, cfg, repo) if verdict.get("ok") else (False, "; ".join(verdict["problems"])[:300]))
        if ok:
            record_qualified(root, v, "live", proof)
            state["promoted_at"][name] = at.isoformat(timespec="seconds")
            status(root, name, result="updated", current=v, previous=cur, candidate=v, detail=proof)
        else:
            swap_current(root, cur)
            state["blocked"].setdefault(name, {})[v] = "failed after promotion: " + proof
            failed.append((name, root, cur, v, proof))
    write_json_atomic(state_path, state)

    if failed:
        healthy = restart_and_wait(cfg, info)
        for name, root, cur, v, proof in failed:
            back = prod_check(name, info, live=False) if healthy else {"ok": False}
            detail = f"{v} failed ({proof}); back on {cur}" + ("" if back.get("ok") else ", which does not pass either")
            status(root, name, result="rolled_back", current=cur, previous=v, candidate=v, detail=detail[:300])
        if not healthy:
            notify_fallback(cfg, "ezllm down after bridge rollback",
                            "ezllm did not come back healthy after rolling back " + ", ".join(f[0] for f in failed))
            return 2

    for name, (root, cur, v) in passed.items():
        prune(root, cfg["keep"], {current_version(root), cur, v})
    return 0


def init_layout(args):
    """Create the version-folder layout and the current links from what
    production mounts today, without touching production: the old paths stay."""
    cfg = load_config(Path(args.config).expanduser())
    info = inspect(cfg["container"])
    by_dest = {m["Destination"]: Path(m["Source"]) for m in info["Mounts"]}
    legacy = {"claude": "/opt/claude/claude", "codex": "/opt/codex", "agy": "/opt/agy/agy"}
    for name in ([args.bridge] if args.bridge else BRIDGES):
        root = Path(BRIDGES[name]["root"]).expanduser()
        if current_version(root):
            log(f"{name}: current -> {current_version(root)} already")
            continue
        src = by_dest.get(legacy[name])
        if src is None:
            log(f"{name}: production mounts nothing at {legacy[name]}; skipped")
            continue
        # The version comes from the pinned path (claude-2.1.306, 0.163.0/,
        # 1.4.7/agy), never from running the CLI: agy rewrites itself in place
        # when run from a writable folder.
        m = re.search(r"\d+\.\d+\.\d+", src.name if name == "claude" else
                      (src.name if name == "codex" else src.parent.name))
        if not m:
            log(f"{name}: no version in the mounted path {src}; skipped")
            continue
        v = m.group(0)
        verdir = root / v
        if name == "claude" and not verdir.exists():
            verdir.mkdir()
            shutil.copy2(src, verdir / "claude")
        if not (verdir / BRIDGES[name]["bin"]).is_file():
            log(f"{name}: {verdir / BRIDGES[name]['bin']} missing; skipped")
            continue
        set_writable(verdir, False)
        swap_current(root, v)
        log(f"{name}: current -> {v} ({verdir})")
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--config", default="~/.config/ezllm/bridge-update.json")
    ap.add_argument("--bridge", choices=sorted(BRIDGES))
    ap.add_argument("--dry-run", action="store_true", help="discover, stage and gate; never promote")
    ap.add_argument("--force-window", action="store_true", help="act now, outside the window (supervised runs)")
    ap.add_argument("--init", action="store_true", help="create the version layout and current links, then exit")
    args = ap.parse_args()
    lock_path = Path("~/.local/state/ezllm/bridge-update.lock").expanduser()
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            log("another bridge update is running")
            return 0
        return init_layout(args) if args.init else run(args)


if __name__ == "__main__":
    sys.exit(main())
