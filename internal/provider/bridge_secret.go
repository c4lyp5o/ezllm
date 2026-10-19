package provider

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
)

// BridgeSecretHeader carries the process secret that admits a request to the
// in-process web bridges (/internal/claudeweb, /internal/geminiweb). Those
// routes share the public listener, and their account header alone names an
// account whose STORED credential the bridge will use — so the header must not
// be enough. Only this process knows the secret: the web adapters stamp it on
// every hop they rewrite to a bridge, and the bridges refuse anything else.
const BridgeSecretHeader = "X-Ezllm-Bridge-Secret"

// BridgeSecret returns this process's bridge secret, generated on first use.
// It is never persisted or logged: adapter and bridge live in one process, so
// a restart simply mints a new one.
var BridgeSecret = sync.OnceValue(func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("provider: cannot generate bridge secret: " + err.Error())
	}
	return hex.EncodeToString(b)
})

// ValidBridgeSecret reports, in constant time, whether got is this process's
// bridge secret.
func ValidBridgeSecret(got string) bool {
	want := BridgeSecret()
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
