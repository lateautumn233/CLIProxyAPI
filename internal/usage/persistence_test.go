package usage

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPersistenceManagerLoadRecoversFromTempFile(t *testing.T) {
	stats := NewRequestStatistics()
	pm := NewPersistenceManager(stats, t.TempDir(), defaultSaveIntervalSec)

	savedAt := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	payload := persistencePayload{
		Version: 1,
		SavedAt: savedAt,
		Usage:   testStatisticsSnapshot("api-temp", "model-temp", savedAt.Add(-time.Minute), 11),
	}
	mustWritePersistencePayload(t, pm.tempFilePath(), payload)

	pm.Load()

	assertUsageContainsAPI(t, stats.Snapshot(), "api-temp")
	recovered := mustReadPersistencePayload(t, pm.filePath())
	if recovered.SavedAt.UTC() != savedAt {
		t.Fatalf("recovered saved_at = %s, want %s", recovered.SavedAt.UTC(), savedAt)
	}
	if _, err := os.Stat(pm.tempFilePath()); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be consumed, stat err = %v", err)
	}
}

func TestPersistenceManagerLoadRecoversFromBackupWhenPrimaryInvalid(t *testing.T) {
	stats := NewRequestStatistics()
	pm := NewPersistenceManager(stats, t.TempDir(), defaultSaveIntervalSec)

	if err := os.WriteFile(pm.filePath(), []byte("not-json"), 0o644); err != nil {
		t.Fatalf("failed to seed invalid primary file: %v", err)
	}

	savedAt := time.Date(2026, 3, 7, 11, 0, 0, 0, time.UTC)
	backup := persistencePayload{
		Version: 1,
		SavedAt: savedAt,
		Usage:   testStatisticsSnapshot("api-backup", "model-backup", savedAt.Add(-time.Minute), 22),
	}
	mustWritePersistencePayload(t, pm.backupFilePath(), backup)

	pm.Load()

	assertUsageContainsAPI(t, stats.Snapshot(), "api-backup")
	recovered := mustReadPersistencePayload(t, pm.filePath())
	if recovered.SavedAt.UTC() != savedAt {
		t.Fatalf("recovered saved_at = %s, want %s", recovered.SavedAt.UTC(), savedAt)
	}
}

func TestPersistenceManagerLoadPrefersNewerTempOverPrimary(t *testing.T) {
	stats := NewRequestStatistics()
	pm := NewPersistenceManager(stats, t.TempDir(), defaultSaveIntervalSec)

	primarySavedAt := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	tempSavedAt := primarySavedAt.Add(5 * time.Minute)

	mustWritePersistencePayload(t, pm.filePath(), persistencePayload{
		Version: 1,
		SavedAt: primarySavedAt,
		Usage:   testStatisticsSnapshot("api-primary", "model-primary", primarySavedAt.Add(-time.Minute), 33),
	})
	mustWritePersistencePayload(t, pm.tempFilePath(), persistencePayload{
		Version: 1,
		SavedAt: tempSavedAt,
		Usage:   testStatisticsSnapshot("api-temp-new", "model-temp-new", tempSavedAt.Add(-time.Minute), 44),
	})

	pm.Load()

	snapshot := stats.Snapshot()
	assertUsageContainsAPI(t, snapshot, "api-temp-new")
	if _, ok := snapshot.APIs["api-primary"]; ok {
		t.Fatalf("expected newer temp snapshot to win over primary")
	}

	recovered := mustReadPersistencePayload(t, pm.filePath())
	if recovered.SavedAt.UTC() != tempSavedAt {
		t.Fatalf("primary saved_at after recovery = %s, want %s", recovered.SavedAt.UTC(), tempSavedAt)
	}
}

func TestPersistenceManagerSaveRotatesPrimaryToBackup(t *testing.T) {
	stats := NewRequestStatistics()
	pm := NewPersistenceManager(stats, t.TempDir(), defaultSaveIntervalSec)

	existingSavedAt := time.Date(2026, 3, 7, 13, 0, 0, 0, time.UTC)
	mustWritePersistencePayload(t, pm.filePath(), persistencePayload{
		Version: 1,
		SavedAt: existingSavedAt,
		Usage:   testStatisticsSnapshot("api-old", "model-old", existingSavedAt.Add(-time.Minute), 55),
	})

	if result := stats.MergeSnapshot(testStatisticsSnapshot("api-new", "model-new", existingSavedAt.Add(time.Minute), 66)); result.Added != 1 {
		t.Fatalf("MergeSnapshot added = %d, want 1", result.Added)
	}

	pm.save()

	backup := mustReadPersistencePayload(t, pm.backupFilePath())
	if backup.SavedAt.UTC() != existingSavedAt {
		t.Fatalf("backup saved_at = %s, want %s", backup.SavedAt.UTC(), existingSavedAt)
	}
	if _, ok := backup.Usage.APIs["api-old"]; !ok {
		t.Fatalf("backup payload missing previous primary snapshot")
	}

	current := mustReadPersistencePayload(t, pm.filePath())
	if _, ok := current.Usage.APIs["api-new"]; !ok {
		t.Fatalf("primary payload missing latest snapshot")
	}
}

func testStatisticsSnapshot(apiName, modelName string, ts time.Time, totalTokens int64) StatisticsSnapshot {
	detail := RequestDetail{
		Timestamp: ts.UTC(),
		Source:    "test",
		AuthIndex: "0",
		Tokens: TokenStats{
			InputTokens:  totalTokens / 2,
			OutputTokens: totalTokens - (totalTokens / 2),
			TotalTokens:  totalTokens,
		},
	}

	return StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			apiName: {
				Models: map[string]ModelSnapshot{
					modelName: {
						Details: []RequestDetail{detail},
					},
				},
			},
		},
	}
}

func mustWritePersistencePayload(t *testing.T, path string, payload persistencePayload) {
	t.Helper()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("failed to write payload %s: %v", path, err)
	}
}

func mustReadPersistencePayload(t *testing.T, path string) persistencePayload {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read payload %s: %v", path, err)
	}

	var payload persistencePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("failed to parse payload %s: %v", path, err)
	}
	return payload
}

func assertUsageContainsAPI(t *testing.T, snapshot StatisticsSnapshot, apiName string) {
	t.Helper()

	if snapshot.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	if _, ok := snapshot.APIs[apiName]; !ok {
		t.Fatalf("snapshot missing api %q", apiName)
	}
}
