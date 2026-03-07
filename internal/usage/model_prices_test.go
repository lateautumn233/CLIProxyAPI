package usage

import (
	"testing"
)

func TestModelPriceStoreSaveAndLoad(t *testing.T) {
	path := t.TempDir() + `\model-prices.json`
	store := NewModelPriceStore(path)

	savedAt, err := store.Save(map[string]ModelPrice{
		"gpt-5.4": {
			Prompt:     2.5,
			Completion: 15,
			Cache:      2.5,
		},
		" bad-entry ": {
			Prompt:     -1,
			Completion: 1,
			Cache:      -2,
		},
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if savedAt.IsZero() {
		t.Fatal("Save() returned zero savedAt")
	}

	loaded := NewModelPriceStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	prices, restoredAt := loaded.Snapshot()
	if restoredAt.IsZero() {
		t.Fatal("Snapshot() restored zero savedAt")
	}
	if got, ok := prices["gpt-5.4"]; !ok {
		t.Fatal(`Snapshot() missing "gpt-5.4"`)
	} else {
		if got.Prompt != 2.5 || got.Completion != 15 || got.Cache != 2.5 {
			t.Fatalf("gpt-5.4 price = %+v, want prompt=2.5 completion=15 cache=2.5", got)
		}
	}
	if got, ok := prices["bad-entry"]; !ok {
		t.Fatal(`Snapshot() missing normalized "bad-entry"`)
	} else {
		if got.Prompt != 0 || got.Completion != 1 || got.Cache != 0 {
			t.Fatalf("bad-entry price = %+v, want prompt=0 completion=1 cache=0", got)
		}
	}
}

func TestModelPriceStoreSaveEmptyClearsState(t *testing.T) {
	path := t.TempDir() + `\model-prices.json`
	store := NewModelPriceStore(path)

	if _, err := store.Save(map[string]ModelPrice{
		"gpt-5.4": {Prompt: 2.5, Completion: 15, Cache: 2.5},
	}); err != nil {
		t.Fatalf("initial Save() error = %v", err)
	}

	savedAt, err := store.Save(map[string]ModelPrice{})
	if err != nil {
		t.Fatalf("empty Save() error = %v", err)
	}
	if !savedAt.IsZero() {
		t.Fatalf("empty Save() savedAt = %v, want zero", savedAt)
	}

	prices, restoredAt := store.Snapshot()
	if len(prices) != 0 {
		t.Fatalf("Snapshot() prices = %+v, want empty", prices)
	}
	if !restoredAt.IsZero() {
		t.Fatalf("Snapshot() savedAt = %v, want zero", restoredAt)
	}
}
