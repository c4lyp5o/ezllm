package compress

import (
	"fmt"
	"strings"
	"testing"
)

// benchConversation is a long agent-style history: prose turns and tool output.
func benchConversation(turns int) []byte {
	prose := strings.Repeat("Basically, I think that we should really just go ahead and update the configuration so that it actually works as expected. ", 20)
	tool := strings.Repeat("ok   build step finished\n", 40)
	msgs := []any{textMsg("system", "You are a helpful assistant.")}
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			textMsg("user", fmt.Sprintf("Turn %d: %s", i, prose)),
			textMsg("assistant", fmt.Sprintf("Answer %d: %s", i, prose)),
			textMsg("tool", tool))
	}
	msgs = append(msgs, textMsg("user", "next"))
	return bodyOf(msgs...)
}

// BenchmarkPipelineFourStages measures a cold four-stage run with the session
// memo (every message is new work).
func BenchmarkPipelineFourStages(b *testing.B) {
	body := benchConversation(60)
	p := memoProfile(Stage{Engine: "rtk"}, Stage{Engine: "lite"}, Stage{Engine: "session_dedup"},
		Stage{Engine: "caveman", Options: map[string]any{"intensity": "standard"}})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ApplyWithSession(body, p, nil, fmt.Sprintf("s%d", i), NewSessionMemo(nil))
	}
}

// BenchmarkProtect measures caveman's span protection on plain prose.
func BenchmarkProtect(b *testing.B) {
	s := strings.Repeat("Basically, I think that we should really just go ahead and update the configuration so that it actually works as expected. ", 150)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		protect(s)
	}
}
