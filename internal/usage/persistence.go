package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	persistenceFileName    = "usage-statistics.json"
	defaultSaveIntervalSec = 300
	minSaveIntervalSec     = 10
)

var (
	defaultPM   *PersistenceManager
	defaultPMMu sync.Mutex
)

// SetDefaultPersistenceManager stores the global persistence manager instance.
func SetDefaultPersistenceManager(pm *PersistenceManager) {
	defaultPMMu.Lock()
	defaultPM = pm
	defaultPMMu.Unlock()
}

// GetDefaultPersistenceManager returns the global persistence manager (may be nil).
func GetDefaultPersistenceManager() *PersistenceManager {
	defaultPMMu.Lock()
	defer defaultPMMu.Unlock()
	return defaultPM
}

// ApplyPersistenceConfig reacts to config changes at runtime.
// When persistence transitions from disabled to enabled, a new manager is created and started.
// When it transitions from enabled to disabled, the existing manager is stopped.
// Interval changes are applied dynamically without restart.
func ApplyPersistenceConfig(enabled bool, intervalSec int) {
	defaultPMMu.Lock()
	defer defaultPMMu.Unlock()

	if enabled {
		if defaultPM != nil {
			defaultPM.SetInterval(intervalSec)
		} else {
			pm := NewPersistenceManager(GetRequestStatistics(), "logs", intervalSec)
			pm.Load()
			pm.Start()
			defaultPM = pm
			log.Infof("usage persistence: started (interval=%ds)", intervalSec)
		}
	} else {
		if defaultPM != nil {
			defaultPM.Stop()
			defaultPM = nil
			log.Info("usage persistence: stopped")
		}
	}
}

type persistencePayload struct {
	Version   int                `json:"version"`
	SavedAt   time.Time          `json:"saved_at"`
	Usage     StatisticsSnapshot `json:"usage"`
}

// PersistenceManager handles periodic saving and one-time loading of usage statistics.
type PersistenceManager struct {
	stats    *RequestStatistics
	dir      string
	interval atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewPersistenceManager creates a new persistence manager.
// dir is the logs directory where the file will be stored.
// intervalSec controls the save period in seconds.
func NewPersistenceManager(stats *RequestStatistics, dir string, intervalSec int) *PersistenceManager {
	if intervalSec < minSaveIntervalSec {
		intervalSec = defaultSaveIntervalSec
	}
	pm := &PersistenceManager{
		stats:  stats,
		dir:    dir,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	pm.interval.Store(int64(intervalSec))
	return pm
}

func (pm *PersistenceManager) filePath() string {
	return filepath.Join(pm.dir, persistenceFileName)
}

// Load reads an existing persistence file and merges it into the in-memory stats.
// Safe to call before Start. If the file does not exist, this is a no-op.
func (pm *PersistenceManager) Load() {
	if pm == nil || pm.stats == nil {
		return
	}
	data, err := os.ReadFile(pm.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Warnf("usage persistence: failed to read %s: %v", pm.filePath(), err)
		return
	}
	if len(data) == 0 {
		return
	}
	var payload persistencePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Warnf("usage persistence: failed to parse %s: %v", pm.filePath(), err)
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		log.Warnf("usage persistence: unsupported version %d in %s", payload.Version, pm.filePath())
		return
	}
	result := pm.stats.MergeSnapshot(payload.Usage)
	log.Infof("usage persistence: loaded %s (added=%d, skipped=%d)", pm.filePath(), result.Added, result.Skipped)
}

// Start begins the periodic save loop. It is safe to call only once.
func (pm *PersistenceManager) Start() {
	if pm == nil {
		return
	}
	go pm.run()
}

// Stop terminates the periodic save loop and performs a final flush.
func (pm *PersistenceManager) Stop() {
	if pm == nil {
		return
	}
	pm.stopOnce.Do(func() {
		close(pm.stopCh)
		<-pm.doneCh
	})
}

// SetInterval updates the save interval dynamically (in seconds).
func (pm *PersistenceManager) SetInterval(sec int) {
	if pm == nil {
		return
	}
	if sec < minSaveIntervalSec {
		sec = minSaveIntervalSec
	}
	pm.interval.Store(int64(sec))
}

func (pm *PersistenceManager) run() {
	defer close(pm.doneCh)

	timer := time.NewTimer(pm.currentInterval())
	defer timer.Stop()

	for {
		select {
		case <-pm.stopCh:
			pm.save()
			return
		case <-timer.C:
			pm.save()
			timer.Reset(pm.currentInterval())
		}
	}
}

func (pm *PersistenceManager) currentInterval() time.Duration {
	sec := pm.interval.Load()
	if sec < minSaveIntervalSec {
		sec = defaultSaveIntervalSec
	}
	return time.Duration(sec) * time.Second
}

func (pm *PersistenceManager) save() {
	if pm.stats == nil {
		return
	}
	snapshot := pm.stats.Snapshot()
	if snapshot.TotalRequests == 0 {
		return
	}

	payload := persistencePayload{
		Version: 1,
		SavedAt: time.Now().UTC(),
		Usage:   snapshot,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Warnf("usage persistence: failed to marshal snapshot: %v", err)
		return
	}

	if err := os.MkdirAll(pm.dir, 0o755); err != nil {
		log.Warnf("usage persistence: failed to create directory %s: %v", pm.dir, err)
		return
	}

	tmpFile := pm.filePath() + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		log.Warnf("usage persistence: failed to write temp file: %v", err)
		return
	}
	if err := os.Rename(tmpFile, pm.filePath()); err != nil {
		log.Warnf("usage persistence: failed to rename temp file: %v", err)
		_ = os.Remove(tmpFile)
		return
	}
}
