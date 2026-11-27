package jsonbody

import "testing"

// A hop's model is written into a copy, never into the shared document: a
// body with a duplicate key takes the re-marshal path, which used to change
// the document every later hop and reader saw.
func TestWithModelLeavesTheDocumentAlone(t *testing.T) {
	d := Open([]byte(`{"model":"orig","x":1,"x":2}`))
	if _, err := d.WithModel("hop-1"); err != nil {
		t.Fatal(err)
	}
	if got := d.Map()["model"]; got != "orig" {
		t.Fatalf("document model = %v, want orig", got)
	}
}
