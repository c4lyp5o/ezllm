package settings

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

type memoryStore struct {
	state State
	fail  bool
}

func (s *memoryStore) LoadSettings(context.Context) (State, error) { return s.state, nil }
func (s *memoryStore) SaveSettings(_ context.Context, v State, revision int64) error {
	if s.fail {
		return errors.New("disk full")
	}
	if s.state.Revision != revision {
		return ErrConflict
	}
	s.state = v
	return nil
}
func patch(v string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(v), &m)
	return m
}
func TestSettingsPrecedenceResetAndFailure(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{}
	base := Defaults()
	base.Retries = 5
	base.Translation = false
	m, err := New(ctx, store, base, map[string]string{"retries": "yaml", "translation_enabled": "environment"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Patch(ctx, 0, patch(`{"retries":0}`)); err != nil {
		t.Fatal(err)
	}
	if m.Current().Retries != 0 || m.View().Fields["retries"].Source != "settings" {
		t.Fatal(m.View())
	}
	restarted, err := New(ctx, store, base, map[string]string{"retries": "yaml"})
	if err != nil || restarted.Current().Retries != 0 {
		t.Fatal("restart lost settings", err)
	}
	for _, changes := range []string{`{"translation_enabled":true,"retries":8}`, `{"unknown":true}`, `{"retries":"1"}`, `{"catalog_interval":"1s"}`, `{"usage_timezone":"wrong/zone"}`} {
		if _, err = m.Patch(ctx, 1, patch(changes)); err == nil {
			t.Fatalf("accepted %s", changes)
		}
		if m.Current().Retries != 0 || m.View().Revision != 1 {
			t.Fatal("partial update")
		}
	}
	if _, err = m.Patch(ctx, 0, patch(`{"retries":1}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	store.fail = true
	if _, err = m.Patch(ctx, 1, patch(`{"retries":2}`)); err == nil {
		t.Fatal("accepted failed write")
	}
	if m.Current().Retries != 0 {
		t.Fatal("published failed write")
	}
	store.fail = false
	if _, err = m.Patch(ctx, 1, patch(`{"retries":null}`)); err != nil {
		t.Fatal(err)
	}
	if m.Current().Retries != 5 || m.View().Fields["retries"].Source != "yaml" {
		t.Fatal(m.View())
	}
}
func TestSettingsSnapshotAndConcurrentUpdates(t *testing.T) {
	ctx := context.Background()
	m, err := New(ctx, &memoryStore{}, Defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	admitted := With(ctx, m.Current())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = m.Patch(ctx, 0, patch(`{"retries":1}`)); _ = m.Current(); _ = m.View() }()
	}
	wg.Wait()
	if m.View().Revision != 1 {
		t.Fatal(m.View())
	}
	p, _ := From(admitted)
	if p.Retries != 3 || m.Current().Retries != 1 {
		t.Fatal("policy changed mid request")
	}
}
