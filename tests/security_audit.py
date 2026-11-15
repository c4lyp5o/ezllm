#!/usr/bin/env python3
"""Run permanent, synthetic regressions for the 2026-11-14 security fixes.

Unlike the original vulnerable-behavior reproductions, PASS now means that the
regression protections hold. No generated Go files, real credentials, live
services, or paid inference requests are used. All Go tests use -short.
"""

from pathlib import Path
import subprocess
import sys


def main():
    root = Path(__file__).resolve().parents[1]
    result = subprocess.run(
        ["go", "test", "-short", "./internal/server", "./internal/proxy",
         "./internal/codexbridge", "./internal/config", "-count=1", "-v",
         "-run", "^TestSecurityRegression|^TestSecurityRequestLimits"],
        cwd=root, timeout=120)
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
