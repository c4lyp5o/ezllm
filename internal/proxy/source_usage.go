package proxy

import (
	"bytes"
	"io"
)

// sourceUsageReader observes bytes as the codec consumes them, before client
// projection. It never reads ahead or retains more than one bounded SSE line.
type sourceUsageReader struct {
	io.ReadCloser
	tap      *UsageTapper
	line     []byte
	overlong bool
}

func (r *sourceUsageReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	data := p[:n]
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		complete := end >= 0
		if !complete {
			end = len(data) - 1
		}
		part := data[:end+1]
		data = data[end+1:]
		if !r.overlong {
			if len(r.line)+len(part) > maxScanToken {
				r.line = nil
				r.overlong = true
				r.tap.event = nil
				r.tap.skipEvent = true
			} else {
				r.line = append(r.line, part...)
			}
		}
		if complete {
			if !r.overlong {
				r.tap.Observe(r.line)
			}
			r.line = nil
			r.overlong = false
		}
	}
	if err != nil && len(r.line) > 0 {
		r.tap.Observe(r.line)
		r.line = nil
	}
	return n, err
}
