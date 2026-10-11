package server

import (
	"encoding/json"
	"io"
)

// jsonObject streams a JSON object one field at a time.
//
// A hand-rolled comma is the only way to get this shape wrong, and the failure
// mode is ugly: one stray separator produces a body that no JSON parser accepts
// — an undownloadable disaster-recovery file, which is worse than a slow one. So
// the comma state lives HERE, encapsulated, instead of being re-derived at
// every call site.
type jsonObject struct {
	w   io.Writer
	enc *json.Encoder
	n   int
}

func newJSONObject(w io.Writer) *jsonObject {
	return &jsonObject{w: w, enc: json.NewEncoder(w)}
}

// field writes ,"name":<marshalled value>.
func (o *jsonObject) field(name string, v any) error {
	if err := o.separate(); err != nil {
		return err
	}
	nb, err := json.Marshal(name)
	if err != nil {
		return err
	}
	if _, err := o.w.Write(nb); err != nil {
		return err
	}
	if _, err := io.WriteString(o.w, ":"); err != nil {
		return err
	}
	return o.enc.Encode(v)
}

// beginField writes the separator and ,"name":, leaving the VALUE to the caller
// — for a value too large to marshal in one piece (a streamed array).
func (o *jsonObject) beginField(name string) error {
	if err := o.separate(); err != nil {
		return err
	}
	nb, err := json.Marshal(name)
	if err != nil {
		return err
	}
	if _, err := o.w.Write(nb); err != nil {
		return err
	}
	_, err = io.WriteString(o.w, ":")
	return err
}

// separate emits the comma before any field but the first.
func (o *jsonObject) separate() error {
	if o.n == 0 {
		o.n++
		return nil
	}
	_, err := io.WriteString(o.w, ",")
	return err
}

// raw writes pre-formed JSON. Only valid between fields.
func (o *jsonObject) raw(s string) error {
	_, err := io.WriteString(o.w, s)
	return err
}
