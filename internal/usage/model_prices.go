package usage

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ModelPriceFileName   = "model-prices.json"
	modelPriceTempSuffix = ".tmp"
)

// ModelPrice stores per-1M-token pricing used for usage cost estimation.
type ModelPrice struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	Cache      float64 `json:"cache"`
}

type modelPricesPayload struct {
	Version int                   `json:"version"`
	SavedAt time.Time             `json:"saved_at"`
	Prices  map[string]ModelPrice `json:"prices"`
}

// ModelPriceStore persists user-defined model prices to a local JSON file.
type ModelPriceStore struct {
	path string

	mu      sync.RWMutex
	prices  map[string]ModelPrice
	savedAt time.Time
}

// NewModelPriceStore creates a file-backed model price store.
func NewModelPriceStore(path string) *ModelPriceStore {
	return &ModelPriceStore{
		path:   path,
		prices: make(map[string]ModelPrice),
	}
}

// Snapshot returns a defensive copy of the stored prices and the last save time.
func (s *ModelPriceStore) Snapshot() (map[string]ModelPrice, time.Time) {
	if s == nil {
		return map[string]ModelPrice{}, time.Time{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return cloneModelPrices(s.prices), s.savedAt
}

// Load hydrates the store from disk when the persistence file exists.
func (s *ModelPriceStore) Load() error {
	if s == nil || s.path == "" {
		return nil
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var payload modelPricesPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	s.mu.Lock()
	s.prices = NormalizeModelPrices(payload.Prices)
	s.savedAt = payload.SavedAt.UTC()
	s.mu.Unlock()
	return nil
}

// Save replaces the stored prices and persists them atomically to disk.
func (s *ModelPriceStore) Save(prices map[string]ModelPrice) (time.Time, error) {
	if s == nil {
		return time.Time{}, errors.New("model price store is nil")
	}

	normalized := NormalizeModelPrices(prices)
	if len(normalized) == 0 {
		if err := s.clearFile(); err != nil {
			return time.Time{}, err
		}
		s.mu.Lock()
		s.prices = make(map[string]ModelPrice)
		s.savedAt = time.Time{}
		s.mu.Unlock()
		return time.Time{}, nil
	}

	savedAt := time.Now().UTC()
	payload := modelPricesPayload{
		Version: 1,
		SavedAt: savedAt,
		Prices:  normalized,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return time.Time{}, err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return time.Time{}, err
	}

	tempPath := s.path + modelPriceTempSuffix
	if err := os.WriteFile(tempPath, data, 0o644); err != nil {
		return time.Time{}, err
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		if removeErr := os.Remove(s.path); removeErr == nil || os.IsNotExist(removeErr) {
			if retryErr := os.Rename(tempPath, s.path); retryErr == nil {
				s.mu.Lock()
				s.prices = cloneModelPrices(normalized)
				s.savedAt = savedAt
				s.mu.Unlock()
				return savedAt, nil
			} else {
				_ = os.Remove(tempPath)
				return time.Time{}, retryErr
			}
		}
		_ = os.Remove(tempPath)
		return time.Time{}, err
	}

	s.mu.Lock()
	s.prices = cloneModelPrices(normalized)
	s.savedAt = savedAt
	s.mu.Unlock()
	return savedAt, nil
}

// NormalizeModelPrices trims model names and clamps invalid numeric values.
func NormalizeModelPrices(prices map[string]ModelPrice) map[string]ModelPrice {
	if len(prices) == 0 {
		return map[string]ModelPrice{}
	}

	normalized := make(map[string]ModelPrice, len(prices))
	for model, price := range prices {
		name := strings.TrimSpace(model)
		if name == "" {
			continue
		}
		normalized[name] = ModelPrice{
			Prompt:     normalizeModelPriceValue(price.Prompt),
			Completion: normalizeModelPriceValue(price.Completion),
			Cache:      normalizeModelPriceValue(price.Cache),
		}
	}
	return normalized
}

func normalizeModelPriceValue(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func cloneModelPrices(prices map[string]ModelPrice) map[string]ModelPrice {
	if len(prices) == 0 {
		return map[string]ModelPrice{}
	}

	cloned := make(map[string]ModelPrice, len(prices))
	for model, price := range prices {
		cloned[model] = price
	}
	return cloned
}

func (s *ModelPriceStore) clearFile() error {
	if s == nil || s.path == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	tempPath := s.path + modelPriceTempSuffix
	if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
