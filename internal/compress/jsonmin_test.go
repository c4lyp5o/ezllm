package compress

import (
	"strings"
	"testing"
)

const prettyJSON = `{
  "zeta": 1,
  "alpha": {
    "big": 12345678901234567890,
    "float": 1.0,
    "exp": 1e3,
    "esc": "café <b> & \"q\"",
    "list": [
      1,
      2
    ]
  },
  "zeta": "dup kept"
}`

// The minified form is the input minus insignificant whitespace — nothing
// else: key order, duplicate keys, number spellings and string escapes are
// byte-identical, in every shape, for log, search and unknown tools.
func TestJSONMinCompactsPrettyJSONExactly(t *testing.T) {
	want := `{"zeta":1,"alpha":{"big":12345678901234567890,"float":1.0,"exp":1e3,"esc":"café <b> & \"q\"","list":[1,2]},"zeta":"dup kept"}`
	for _, tool := range []string{"bash", "grep", "deploy_status"} {
		sameInEveryShape(t, "jsonmin "+tool, toolOutputAfter(t, Stage{Engine: "jsonmin"}, tool, prettyJSON), want)
	}
	// Surrounding whitespace goes with it; an array payload compacts too.
	sameInEveryShape(t, "jsonmin array", toolOutputAfter(t, Stage{Engine: "jsonmin"}, "bash", "\n  [\n    {\"a\": 1},\n    {\"b\": [ true, null ]}\n  ]\n"),
		`[{"a":1},{"b":[true,null]}]`)
}

// Never: a file read (the next edit quotes it back), text that is not one
// whole JSON value, or JSON that is already compact.
func TestJSONMinLeavesFileReadsProseAndCompactAlone(t *testing.T) {
	sameInEveryShape(t, "jsonmin file read", toolOutputAfter(t, Stage{Engine: "jsonmin"}, "read_file", prettyJSON), prettyJSON)
	for _, in := range []string{
		"result:\n" + prettyJSON,             // text around the JSON
		"{\n  \"a\": 1,\n}",                  // invalid (trailing comma)
		`{"already":"compact","n":[1,2]}`,    // nothing to remove
		"{\n  \"a\": 1\n}\n{\n  \"b\": 2\n}", // two values (JSON lines)
	} {
		sameInEveryShape(t, "jsonmin "+strings.SplitN(in, "\n", 2)[0], toolOutputAfter(t, Stage{Engine: "jsonmin"}, "bash", in), in)
	}
}
