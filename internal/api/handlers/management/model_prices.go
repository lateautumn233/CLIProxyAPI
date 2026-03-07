package management

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

type usageModelPricesResponse struct {
	Version int                         `json:"version"`
	SavedAt *time.Time                  `json:"saved_at,omitempty"`
	Prices  map[string]usage.ModelPrice `json:"prices"`
}

type usageModelPricesRequest struct {
	Prices map[string]usageModelPriceInput `json:"prices"`
}

type usageModelPriceInput struct {
	Prompt     *float64 `json:"prompt"`
	Completion *float64 `json:"completion"`
	Cache      *float64 `json:"cache"`
}

// GetUsageModelPrices returns the locally persisted model price overrides.
func (h *Handler) GetUsageModelPrices(c *gin.Context) {
	store := h.getModelPriceStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "model price store unavailable"})
		return
	}

	prices, savedAt := store.Snapshot()
	c.JSON(http.StatusOK, newUsageModelPricesResponse(prices, savedAt))
}

// PutUsageModelPrices replaces the locally persisted model price overrides.
func (h *Handler) PutUsageModelPrices(c *gin.Context) {
	store := h.getModelPriceStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "model price store unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	prices, err := parseUsageModelPricesRequest(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	savedAt, err := store.Save(prices)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist model prices"})
		return
	}

	c.JSON(http.StatusOK, newUsageModelPricesResponse(prices, savedAt))
}

func newUsageModelPricesResponse(prices map[string]usage.ModelPrice, savedAt time.Time) usageModelPricesResponse {
	response := usageModelPricesResponse{
		Version: 1,
		Prices:  usage.NormalizeModelPrices(prices),
	}
	if !savedAt.IsZero() {
		savedAtUTC := savedAt.UTC()
		response.SavedAt = &savedAtUTC
	}
	return response
}

func parseUsageModelPricesRequest(data []byte) (map[string]usage.ModelPrice, error) {
	var wrapped usageModelPricesRequest
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Prices != nil {
		return normalizeUsageModelPriceInputs(wrapped.Prices), nil
	}

	var direct map[string]usageModelPriceInput
	if err := json.Unmarshal(data, &direct); err != nil {
		return nil, err
	}
	return normalizeUsageModelPriceInputs(direct), nil
}

func normalizeUsageModelPriceInputs(items map[string]usageModelPriceInput) map[string]usage.ModelPrice {
	normalized := make(map[string]usage.ModelPrice, len(items))
	for model, item := range items {
		name := strings.TrimSpace(model)
		if name == "" {
			continue
		}

		prompt := normalizeUsageModelPriceValue(item.Prompt)
		completion := normalizeUsageModelPriceValue(item.Completion)
		cache := prompt
		if item.Cache != nil {
			cache = normalizeUsageModelPriceValue(item.Cache)
		}

		normalized[name] = usage.ModelPrice{
			Prompt:     prompt,
			Completion: completion,
			Cache:      cache,
		}
	}
	return usage.NormalizeModelPrices(normalized)
}

func normalizeUsageModelPriceValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0
	}
	return *value
}

func (h *Handler) getModelPriceStore() *usage.ModelPriceStore {
	if h == nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.modelPriceStore != nil {
		return h.modelPriceStore
	}

	store := usage.NewModelPriceStore(h.modelPriceStorePath())
	if err := store.Load(); err != nil {
		log.Warnf("management: failed to load model prices: %v", err)
	}
	h.modelPriceStore = store
	return h.modelPriceStore
}

func (h *Handler) modelPriceStorePath() string {
	if h != nil && strings.TrimSpace(h.logDir) != "" {
		return filepath.Join(h.logDir, usage.ModelPriceFileName)
	}
	if h != nil && strings.TrimSpace(h.configFilePath) != "" {
		return filepath.Join(filepath.Dir(h.configFilePath), "logs", usage.ModelPriceFileName)
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "logs", usage.ModelPriceFileName)
	}
	return usage.ModelPriceFileName
}
