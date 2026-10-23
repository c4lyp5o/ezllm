#!/usr/bin/env python3
"""Produce a new, verified SQLite/key backup directory using the online backup API."""
import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import sys
import time


def sync_write(path, data):
    with path.open("xb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())


def backup(db_path, key_path, output_path, timeout):
    source = db_path.resolve(strict=True)
    key = key_path.read_bytes()
    if not key.strip():
        raise ValueError("master-key file is empty")
    # Never merge into or replace an existing backup, even if it is incomplete.
    output_path.mkdir(mode=0o700)
    database = output_path / "ezllm.sqlite"
    deadline = time.monotonic() + timeout

    def progress(status, remaining, total):
        if time.monotonic() > deadline:
            raise TimeoutError("SQLite backup timed out; directory is incomplete")

    with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as src:
        with closing(sqlite3.connect(database)) as dst:
            src.backup(dst, pages=256, progress=progress, sleep=0.05)
            if dst.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
                raise ValueError("backup integrity check failed")
            schema = dst.execute("SELECT MAX(version) FROM schema_version").fetchone()[0]
    with database.open("rb") as stream:
        os.fsync(stream.fileno())
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    sync_write(output_path / "ezllm-master.key", key)
    manifest = {"format": "ezllm-sqlite-backup", "format_version": 1,
                "complete": True, "schema_version": schema,
                "database": database.name, "master_key": "ezllm-master.key",
                "database_sha256": digest}
    sync_write(output_path / "COMPLETE.json", (json.dumps(manifest) + "\n").encode())
    if os.name != "nt":
        directory = os.open(output_path, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", type=Path, required=True)
    parser.add_argument("--key", type=Path, required=True,
                        help="active key file (must match EZLLM_MASTER_KEY if env overrides it)")
    parser.add_argument("--output", type=Path, required=True, help="new directory; must not exist")
    parser.add_argument("--timeout", type=float, default=300)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout must be positive")
    os.umask(0o077)
    try:
        backup(args.db, args.key, args.output, args.timeout)
    except (OSError, ValueError, sqlite3.Error) as error:
        print(f"backup failed: {error}", file=sys.stderr)
        return 1
    print(f"backup complete: {args.output / 'COMPLETE.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
