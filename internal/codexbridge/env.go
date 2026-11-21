package codexbridge

import (
	"os"
	"strings"
)

// childEnv is the process environment for one Codex child. It is an ALLOWLIST
// of what the CLI needs to start and reach the network from a container —
// everything else in ezllm's environment (EZLLM_MASTER_KEY, provider and cloud
// credentials, OPENAI_*) stays out of a process that has no reason to see it.
// HOME is the per-request temp dir; CODEX_HOME is the subscription login, the
// only credential path.
func childEnv(home, codexHome string) []string {
	env := make([]string, 0, 24)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && (childEnvPass[key] || strings.HasPrefix(key, "LC_")) {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+home, "CODEX_HOME="+codexHome)
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
