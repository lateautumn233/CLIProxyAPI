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
	persistenceFileName     = "usage-statistics.json"
	persistenceTempSuffix   = ".tmp"
	persistenceBackupSuffix = ".bak"
	defaultSaveIntervalSec  = 300
	minSaveIntervalSec      = 10
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
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

type persistenceCandidate struct {
	path     string
	label    string
	data     []byte
	payload  persistencePayload
	savedAt  time.Time
	priority int
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

func (pm *PersistenceManager) tempFilePath() string {
	return pm.filePath() + persistenceTempSuffix
}

func (pm *PersistenceManager) backupFilePath() string {
	return pm.filePath() + persistenceBackupSuffix
}

// Load reads an existing persistence file and merges it into the in-memory stats.
// Safe to call before Start. If the file does not exist, this is a no-op.
func (pm *PersistenceManager) Load() {
	if pm == nil || pm.stats == nil {
		return
	}

	candidate := pm.selectStartupCandidate()
	if candidate == nil {
		return
	}

	if candidate.path != pm.filePath() {
		if err := pm.writePrimaryData(candidate.data, false); err != nil {
			log.Warnf("usage persistence: failed to restore %s from %s: %v", pm.filePath(), candidate.path, err)
		} else {
			log.Infof("usage persistence: recovered %s from %s", pm.filePath(), candidate.path)
		}
	}

	result := pm.stats.MergeSnapshot(candidate.payload.Usage)
	log.Infof("usage persistence: loaded %s (added=%d, skipped=%d)", candidate.path, result.Added, result.Skipped)
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

	if err := pm.writePrimaryData(data, true); err != nil {
		log.Warnf("usage persistence: failed to persist snapshot: %v", err)
		return
	}
}

func (pm *PersistenceManager) selectStartupCandidate() *persistenceCandidate {
	candidates := []*persistenceCandidate{
		pm.loadCandidate(pm.filePath(), "primary", 3),
		pm.loadCandidate(pm.tempFilePath(), "temp", 2),
		pm.loadCandidate(pm.backupFilePath(), "backup", 1),
	}

	var best *persistenceCandidate
	for _, candidate := range candidates {
		best = betterPersistenceCandidate(best, candidate)
	}
	return best
}

func (pm *PersistenceManager) loadCandidate(path, label string, priority int) *persistenceCandidate {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("usage persistence: failed to read %s file %s: %v", label, path, err)
		}
		return nil
	}
	if len(data) == 0 {
		log.Warnf("usage persistence: ignoring empty %s file %s", label, path)
		return nil
	}

	var payload persistencePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Warnf("usage persistence: failed to parse %s file %s: %v", label, path, err)
		return nil
	}
	if payload.Version != 0 && payload.Version != 1 {
		log.Warnf("usage persistence: unsupported version %d in %s file %s", payload.Version, label, path)
		return nil
	}

	savedAt := payload.SavedAt.UTC()
	if savedAt.IsZero() {
		if info, err := os.Stat(path); err == nil {
			savedAt = info.ModTime().UTC()
		}
	}

	return &persistenceCandidate{
		path:     path,
		label:    label,
		data:     data,
		payload:  payload,
		savedAt:  savedAt,
		priority: priority,
	}
}

func betterPersistenceCandidate(current, next *persistenceCandidate) *persistenceCandidate {
	if next == nil {
		return current
	}
	if current == nil {
		return next
	}
	if next.savedAt.After(current.savedAt) {
		return next
	}
	if current.savedAt.After(next.savedAt) {
		return current
	}
	if next.priority > current.priority {
		return next
	}
	return current
}

func (pm *PersistenceManager) writePrimaryData(data []byte, rotateCurrentToBackup bool) error {
	if err := os.MkdirAll(pm.dir, 0o755); err != nil {
		return err
	}

	tmpFile := pm.tempFilePath()
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return err
	}

	primaryFile := pm.filePath()
	backupFile := pm.backupFilePath()
	movedPrimaryToBackup := false

	if rotateCurrentToBackup {
		if _, err := os.Stat(primaryFile); err == nil {
			if err := os.Remove(backupFile); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(primaryFile, backupFile); err != nil {
				return err
			}
			movedPrimaryToBackup = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if err := os.Rename(tmpFile, primaryFile); err != nil {
		if !rotateCurrentToBackup {
			if removeErr := os.Remove(primaryFile); removeErr == nil || os.IsNotExist(removeErr) {
				if retryErr := os.Rename(tmpFile, primaryFile); retryErr == nil {
					return nil
				}
			}
		}
		if rotateCurrentToBackup && movedPrimaryToBackup {
			if restoreErr := os.Rename(backupFile, primaryFile); restoreErr != nil && !os.IsNotExist(restoreErr) {
				log.Warnf("usage persistence: failed to restore primary file from backup %s: %v", backupFile, restoreErr)
			}
		}
		return err
	}

	return nil
}
