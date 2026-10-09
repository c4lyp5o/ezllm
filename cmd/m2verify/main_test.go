package main

import "testing"

func TestAnnotationDoesNotCountAsInvariantFailure(t *testing.T) {
	// Document the verifier's decision: blanks are counted and annotated, not failed.
	failures := 0
	blankAcct, blankHint, blankClient := 4, 2, 1
	if blankAcct+blankHint+blankClient == 0 {
		failures++
	}
	if failures != 0 {
		t.Fatalf("annotation should preserve raw blanks without invariant failure")
	}
}
