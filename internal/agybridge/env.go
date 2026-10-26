package agybridge

import (
	"os"
	"strings"
)

// childEnv is the process environment for one agy child. It is an ALLOWLIST:
// everything else in ezllm's environment (EZLLM_MASTER_KEY, provider and cloud
// credentials, GEMINI_API_KEY) stays out of a process that has no reason to see
// it. Two omissions are load-bearing: no DBUS_SESSION_BUS_ADDRESS, so agy uses
// its token file and never the desktop keyring (measured on 1.4.7: the CLI
// bypasses the keyring when no session bus is present), and no GEMINI_API_KEY
// or AGY_ADC_AUTH, so the subscription login is the only auth path.
func childEnv(home string) []string {
	env := make([]string, 0, 24)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && (childEnvPass[key] || strings.HasPrefix(key, "LC_")) {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+home, "AGY_CLI_HIDE_ACCOUNT_INFO=1", "NO_COLOR=1")
}

// childEnvPass are inherited verbatim: process identity, locale, temp dir, CA
// trust and outbound proxy settings.
var childEnvPass = map[string]bool{
	"PATH": true, "USER": true, "LOGNAME": true,
	"LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
}
