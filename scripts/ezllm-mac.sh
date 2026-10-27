#!/usr/bin/env bash
# ezllm on macOS — install the native release binary and run it as a launchd
# user agent. No Docker, no Go toolchain, no Homebrew: the release binary is
# static (CGO-free, pure-Go SQLite) and the dashboard SPA is embedded.
#
#   ./scripts/ezllm-mac.sh install      download + verify + lay out ~/ezllm, write the plist
#   ./scripts/ezllm-mac.sh start        load the agent and wait for /healthz
#   ./scripts/ezllm-mac.sh status       launchd state + /healthz + last log lines
#   ./scripts/ezllm-mac.sh restart      kickstart -k
#   ./scripts/ezllm-mac.sh stop         unload the agent (data stays)
#   ./scripts/ezllm-mac.sh logs         tail -f the service log
#   ./scripts/ezllm-mac.sh uninstall    unload + remove the plist (add --purge for data)
#
# Overridable environment (all optional):
#   EZLLM_VERSION   release to install            (default 1.3.2)
#   EZLLM_PREFIX    install root                  (default ~/ezllm)
#   EZLLM_ADDR      listen address                (default 127.0.0.1:20129)
#   EZLLM_USAGE_TZ  report/schedule timezone      (default Asia/Kuala_Lumpur)
#   EZLLM_LOG_LEVEL debug|info|warn|error         (default info)
#   EZLLM_ARCH      arm64|amd64, skips autodetect (testing only)
#   EZLLM_TARBALL   local tarball, skips the download and its checksum
set -euo pipefail

VERSION="${EZLLM_VERSION:-1.3.2}"; VERSION="${VERSION#v}"   # accept 1.3.2 or v1.3.2
TAG="v${VERSION}"
PREFIX="${EZLLM_PREFIX:-$HOME/ezllm}"
LABEL="com.ezllm.gateway"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BIN="$PREFIX/bin/ezllm"
CONF="$PREFIX/config.yaml"
LOGS="$PREFIX/logs"
ADDR="${EZLLM_ADDR:-127.0.0.1:20129}"
TZNAME="${EZLLM_USAGE_TZ:-Asia/Kuala_Lumpur}"
HOST="https://github.com/c4lyp5o/ezllm/releases/download/${TAG}"
TMP=""   # set in install_bin; the EXIT trap must see it at top level

# ADDR is what we BIND; 0.0.0.0 and :: are not connectable hosts, so health
# probes always go to loopback on the same port.
probe_url() {
    local port="${ADDR##*:}"
    case "${ADDR%:*}" in
        0.0.0.0|"::"|"[::]"|"") printf 'http://127.0.0.1:%s' "$port" ;;
        *) printf 'http://%s' "$ADDR" ;;
    esac
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
note() { printf '  %s\n' "$*"; }

# Apple Silicon under a Rosetta shell reports x86_64 with proc_translated=1 —
# that must still get the NATIVE arm64 build, not an Intel one.
detect_arch() {
    if [ -n "${EZLLM_ARCH:-}" ]; then printf '%s' "$EZLLM_ARCH"; return; fi
    case "$(uname -m)" in
        arm64) printf 'arm64' ;;
        x86_64)
            if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || printf 0)" = "1" ]; then
                printf 'arm64'
            else
                printf 'amd64'
            fi ;;
        *) die "unsupported CPU: $(uname -m)" ;;
    esac
}

sha256_of() { # file -> 64-hex digest
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else sha256sum "$1" | awk '{print $1}'; fi
}

# ---------------------------------------------------------------- install

install_bin() {
    local arch name tarball want got
    arch=$(detect_arch)
    # Release assets carry the git tag, so the name has the "v": ezllm_v1.3.2_…
    name="ezllm_${TAG}_darwin_${arch}"
    tarball="${name}.tar.gz"

    printf 'ezllm %s · darwin/%s · %s\n' "$VERSION" "$arch" "$PREFIX"
    mkdir -p "$PREFIX/bin" "$PREFIX/data" "$LOGS"
    TMP=$(mktemp -d "${TMPDIR:-/tmp}/ezllm.XXXXXX")
    trap 'rm -rf "${TMP:-}"' EXIT
    local tmp="$TMP"

    if [ -n "${EZLLM_TARBALL:-}" ]; then
        cp "$EZLLM_TARBALL" "$tmp/$tarball"
        note "source    $EZLLM_TARBALL (checksum skipped)"
    else
        curl -fsSL --retry 3 -o "$tmp/$tarball" "$HOST/$tarball" \
            || die "download failed: $HOST/$tarball"
        curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$HOST/SHA256SUMS" \
            || die "download failed: $HOST/SHA256SUMS"
        want=$(awk -v f="$tarball" '$2 == f {print $1}' "$tmp/SHA256SUMS")
        [ -n "$want" ] || die "no SHA256SUMS entry for $tarball"
        got=$(sha256_of "$tmp/$tarball")
        [ "$want" = "$got" ] || die "checksum mismatch for $tarball
  expected $want
  got      $got"
        note "checksum  ok"
    fi

    tar xzf "$tmp/$tarball" -C "$tmp"
    [ -f "$tmp/$name/ezllm" ] || die "archive layout unexpected: no $name/ezllm"
    install -m 0755 "$tmp/$name/ezllm" "$BIN"
    # curl does not set the quarantine bit; strip it anyway so a tarball that
    # passed through a browser first still executes.
    xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true
    note "binary    $BIN"

    if [ -f "$CONF" ]; then
        note "config    kept existing $CONF"
    else
        cp "$tmp/$name/config.example.yaml" "$CONF"
        note "config    created $CONF from config.example.yaml"
    fi

    if [ "$(uname -s)" = "Darwin" ]; then
        "$BIN" help >/dev/null || die "binary did not run: $BIN help"
        note "runs      ok ($("$BIN" help | head -1))"
    fi
    write_plist
    printf '\nnext: %s start\n' "$0"
}

write_plist() {
    mkdir -p "$(dirname "$PLIST")"
    cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
        <string>-config</string>
        <string>$CONF</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$PREFIX</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>$HOME</string>
        <key>EZLLM_ADDR</key>
        <string>$ADDR</string>
        <key>EZLLM_DATA_DIR</key>
        <string>$PREFIX/data</string>
        <key>EZLLM_MASTER_KEY_FILE</key>
        <string>$PREFIX/data/ezllm-master.key</string>
        <key>EZLLM_USAGE_TZ</key>
        <string>$TZNAME</string>
        <key>EZLLM_LOG_LEVEL</key>
        <string>${EZLLM_LOG_LEVEL:-info}</string>
        <key>TZ</key>
        <string>$TZNAME</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>$LOGS/ezllm.log</string>
    <key>StandardErrorPath</key>
    <string>$LOGS/ezllm.log</string>
</dict>
</plist>
PLIST_EOF
    if command -v plutil >/dev/null 2>&1; then
        plutil -lint "$PLIST" >/dev/null || die "generated plist is not valid: $PLIST"
    fi
    note "plist     $PLIST"
}

# ---------------------------------------------------------------- service

need_darwin() { [ "$(uname -s)" = "Darwin" ] || die "launchd commands need macOS"; }
need_installed() { [ -x "$BIN" ] || die "not installed: $BIN (run: $0 install)"; }

service_start() {
    need_darwin; need_installed
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST" \
        || launchctl load -w "$PLIST"   # legacy fallback for old macOS
    wait_healthy
}

wait_healthy() {
    local i
    for i in $(seq 1 40); do
        if curl -fsS -m 2 "$(probe_url)/healthz" 2>/dev/null; then
            printf '\n'
            return 0
        fi
        sleep 0.5
    done
    printf '\n'
    die "no answer on $(probe_url)/healthz after 20s — see: $0 logs"
}

service_stop() {
    need_darwin
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null \
        || launchctl unload -w "$PLIST" 2>/dev/null || true
    echo "stopped (data untouched in $PREFIX/data)"
}

service_restart() {
    need_darwin
    launchctl kickstart -k "gui/$(id -u)/$LABEL" || die "kickstart failed — is the agent loaded?"
    wait_healthy
}

service_status() {
    need_darwin
    echo "== launchd"
    launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null \
        | grep -E '^[[:space:]]*(state|pid|last exit code|runs) =' || echo "  not loaded"
    echo "== health"
    curl -fsS -m 3 "$(probe_url)/healthz" 2>/dev/null || echo "  no answer on $(probe_url)/healthz"
    echo "== log (last 10)"
    [ -f "$LOGS/ezllm.log" ] && tail -n 10 "$LOGS/ezllm.log" || echo "  $LOGS/ezllm.log missing"
}

service_logs() { [ -f "$LOGS/ezllm.log" ] || die "no log yet: $LOGS/ezllm.log"; exec tail -f -n 40 "$LOGS/ezllm.log"; }

uninstall() {
    need_darwin
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    rm -f "$PLIST"
    echo "agent removed; binary + data kept in $PREFIX"
    if [ "${1:-}" = "--purge" ]; then
        # The master key lives beside the DB on purpose: one directory to back up.
        # Purging it makes every provider key in an archived DB unreadable.
        rm -rf "$PREFIX/bin" "$PREFIX/logs" "$PREFIX/config.yaml" \
               "$PREFIX/data/ezllm.sqlite" "$PREFIX/data/ezllm.sqlite-wal" \
               "$PREFIX/data/ezllm.sqlite-shm"
        echo "purged binary, config, ledger (master key kept at $PREFIX/data/ezllm-master.key)"
    fi
}

case "${1:-install}" in
    install)   install_bin ;;
    start)     service_start ;;
    stop)      service_stop ;;
    restart)   service_restart ;;
    status)    service_status ;;
    logs)      service_logs ;;
    uninstall) shift || true; uninstall "${1:-}" ;;
    -h|--help|help)
        sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'
        ;;
    *) die "unknown command: $1 (try: $0 help)" ;;
esac
