package bridgeutil

import "testing"

func TestHelpHasFlag(t *testing.T) {
	help := "  --print\n  --json-schema=<file>\tschema\n  --model, -m <id>\n  --printer foo\n"
	for _, f := range []string{"--print", "--json-schema", "--model"} {
		if !HelpHasFlag(help, f) {
			t.Errorf("%s not found", f)
		}
	}
	for _, help := range []string{"  --printer foo\n", "  --sandbox-policy <p>\n", "runs in a sandbox\n"} {
		if HelpHasFlag(help, "--print") || HelpHasFlag(help, "--sandbox") {
			t.Errorf("matched a longer flag or a word in %q", help)
		}
	}
}
