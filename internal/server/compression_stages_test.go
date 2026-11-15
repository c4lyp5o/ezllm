package server

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// An all-zero stage list carries no attribution and is not stored; a list in
// which any stage did something is stored whole.
func TestCompressionStagesStoredOnlyWhenAStageChangedSomething(t *testing.T) {
	idle := []compress.StageStat{{Engine: "ansi", Pre: 137, Post: 137}, {Engine: "fold", Pre: 137, Post: 137, CPUms: 1}}
	var call store.Call
	attachCompressionMetrics(&call, compress.Result{Profile: "combo-safe", Stages: idle})
	if call.CompressionStages != "" {
		t.Fatalf("all-zero stages stored: %s", call.CompressionStages)
	}
	active := append(idle, compress.StageStat{Engine: "jsonmin", Pre: 137, Post: 120, Saved: 17, Fired: 1})
	call = store.Call{}
	attachCompressionMetrics(&call, compress.Result{Profile: "combo-safe", Stages: active})
	if call.CompressionStages == "" {
		t.Fatal("a stage that saved tokens lost its attribution")
	}
}
