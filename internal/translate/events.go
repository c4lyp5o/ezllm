package translate

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// Event separates upstream item identity, public tool identity, source index,
// and encoder-assigned client index. Argument fragments stay strings until the
// block ends; JSON/UTF-8 is never parsed at a transport-read boundary.
type EventKind uint8

const (
	EventStart EventKind = iota + 1
	EventBlockStart
	EventTextDelta
	EventArgumentsDelta
	EventBlockEnd
	EventUsage
	EventFinish
	EventError
)

type Event struct {
	Kind         EventKind
	ID           string
	Created      int64
	Key          string
	ItemID       string
	SourceIndex  int
	ContentIndex int
	Block        Block
	Delta        string
	Usage        UsageIR
	Finish       FinishKind
	StopSequence *string
	Error        *ErrorIR
}

const maxCodecOutput = 32 << 20

// frameScanner implements SSE framing (not line-at-a-time JSON): comments,
// CRLF, optional space after ':', newline-joined data, and bounded frames.
// An EOF tail is accepted for compatibility with existing providers.
type frameScanner struct {
	scanner *bufio.Scanner
	max     int
}

func newFrameScanner(r io.Reader) *frameScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), maxCodecOutput+1)
	return &frameScanner{scanner: sc, max: maxCodecOutput}
}
func (f *frameScanner) Next() (string, []byte, error) {
	var event string
	var data bytes.Buffer
	seen := false
	size := 0
	for f.scanner.Scan() {
		line := f.scanner.Text()
		size += len(line) + 1
		if size > f.max {
			return "", nil, fmt.Errorf("SSE frame exceeds output limit")
		}
		if line == "" {
			if seen {
				return event, data.Bytes(), nil
			}
			size = 0
			event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			if seen {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			seen = true
		}
	}
	if err := f.scanner.Err(); err != nil {
		return "", nil, err
	}
	if seen {
		return event, data.Bytes(), nil
	}
	return "", nil, io.EOF
}
