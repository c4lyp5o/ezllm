package server

import (
	"io"
	"net/http"
)

// readBody reads a request body under a byte cap. tooLarge reports a body
// over max (declared or actual); the caller answers 413.
//
// A known Content-Length within the cap is read into one buffer of exactly
// that size: io.ReadAll grows its buffer repeatedly, which for a 1 MiB prompt
// meant ~26 allocations and twice the bytes. The header is only a size hint,
// never trusted: it is checked against max before allocating, a body shorter
// than declared is an error, and one longer than declared keeps reading under
// the same cap. An unknown length (chunked) takes the capped ReadAll path,
// whose one byte over the cap is what detects an oversized body.
func readBody(r *http.Request, max int64) (body []byte, tooLarge bool, err error) {
	if r.ContentLength > max {
		return nil, true, nil
	}
	if r.ContentLength > 0 {
		body = make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil {
			return nil, false, err
		}
		var probe [1]byte
		n, err := r.Body.Read(probe[:])
		if n == 0 && (err == io.EOF || err == nil) {
			return body, false, nil
		}
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		// More than declared: keep reading, still capped.
		rest, err := io.ReadAll(io.LimitReader(r.Body, max+1-int64(len(body))-int64(n)))
		if err != nil {
			return nil, false, err
		}
		body = append(append(body, probe[:n]...), rest...)
		if int64(len(body)) > max {
			return nil, true, nil
		}
		return body, false, nil
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > max {
		return nil, true, nil
	}
	return body, false, nil
}
