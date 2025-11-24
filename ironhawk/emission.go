package ironhawk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// parseEmitPrefix extracts epoch uuid, epoch counter, commit index and tail payload from a line.
// Supported formats:
// - New:    [emit_epoch=<bucketUUID>:<epochCounter>, emit_commit_index=<N>] <payload>
// - Legacy: [emit_commit_index=<N>] <payload>
func parseEmitPrefix(line string) (epochUUID string, epochCounter uint64, commitIndex uint64, tail string, ok bool) {
	l := strings.TrimSpace(line)
	if l == "" || !strings.HasPrefix(l, "[") {
		return "", 0, 0, line, false
	}
	end := strings.Index(l, "]")
	if end <= 0 {
		return "", 0, 0, line, false
	}
	head := strings.TrimSpace(l[1:end])
	tail = strings.TrimSpace(l[end+1:])
	var (
		eu string
		ec uint64
		ci uint64
		hasCI bool
	)
	parts := strings.Split(head, ",")
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		switch key {
		case "emit_epoch":
			// value format: <uuid>:<counter>
			if idx := strings.LastIndex(val, ":"); idx > 0 {
				eu = strings.TrimSpace(val[:idx])
				if u, err := strconv.ParseUint(strings.TrimSpace(val[idx+1:]), 10, 64); err == nil {
					ec = u
				}
			}
		case "emit_commit_index":
			if u, err := strconv.ParseUint(val, 10, 64); err == nil {
				ci = u
				hasCI = true
			}
		}
	}
	if !hasCI {
		return "", 0, 0, line, false
	}
	return eu, ec, ci, tail, true
}

// DedupeStore tracks last processed commit index for (fp, epochUUID, epochCounter)
// Implementations should be concurrency-safe.
// fp is subscription fingerprint (Fingerprint64 from initial WATCH ACK).
// epochUUID may be empty for legacy servers; epochCounter is 0 in that case.
type DedupeStore interface {
	Get(fp uint64, epochUUID string, epochCounter uint64) (uint64, bool)
	Set(fp uint64, epochUUID string, epochCounter uint64, last uint64)
	Reset(fp uint64, epochUUID string, epochCounter uint64)
}

type ddKey struct {
	FP           uint64  `json:"fp"`
	EpochUUID    string  `json:"epoch_uuid"`
	EpochCounter uint64  `json:"epoch_counter"`
}

type inMemoryDedupe struct {
	mu   sync.RWMutex
	data map[ddKey]uint64
}

func NewInMemoryDedupe() DedupeStore {
	return &inMemoryDedupe{data: make(map[ddKey]uint64)}
}

func (d *inMemoryDedupe) Get(fp uint64, epochUUID string, epochCounter uint64) (uint64, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter}]
	return v, ok
}

func (d *inMemoryDedupe) Set(fp uint64, epochUUID string, epochCounter uint64, last uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter}] = last
}

func (d *inMemoryDedupe) Reset(fp uint64, epochUUID string, epochCounter uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.data, ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter})
}

// fileDedupe persists the store to a JSON file on every Set/Reset.
// Format: {"items":[{"fp":..., "epoch_uuid":..., "epoch_counter":..., "last":...}, ...]}
// This is a simple, best-effort persistence intended for CLI resilience.
type fileDedupe struct {
	mu   sync.RWMutex
	path string
	data map[ddKey]uint64
}

type fileImage struct {
	Items []struct {
		FP           uint64 `json:"fp"`
		EpochUUID    string `json:"epoch_uuid"`
		EpochCounter uint64 `json:"epoch_counter"`
		Last         uint64 `json:"last"`
	} `json:"items"`
}

func NewFileDedupe(path string) (DedupeStore, error) {
	fd := &fileDedupe{path: path, data: make(map[ddKey]uint64)}
	// Best-effort load existing
	_ = fd.load()
	return fd, nil
}

func (d *fileDedupe) Get(fp uint64, epochUUID string, epochCounter uint64) (uint64, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter}]
	return v, ok
}

func (d *fileDedupe) Set(fp uint64, epochUUID string, epochCounter uint64, last uint64) {
	d.mu.Lock()
	d.data[ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter}] = last
	_ = d.saveLocked()
	d.mu.Unlock()
}

func (d *fileDedupe) Reset(fp uint64, epochUUID string, epochCounter uint64) {
	d.mu.Lock()
	delete(d.data, ddKey{FP: fp, EpochUUID: epochUUID, EpochCounter: epochCounter})
	_ = d.saveLocked()
	d.mu.Unlock()
}

func (d *fileDedupe) load() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := os.ReadFile(d.path)
	if err != nil {
		return err
	}
	var img fileImage
	if err := json.Unmarshal(b, &img); err != nil {
		return err
	}
	for _, it := range img.Items {
		k := ddKey{FP: it.FP, EpochUUID: it.EpochUUID, EpochCounter: it.EpochCounter}
		d.data[k] = it.Last
	}
	return nil
}

func (d *fileDedupe) saveLocked() error {
	// ensure dir exists
	_ = os.MkdirAll(filepath.Dir(d.path), 0o755)
	img := fileImage{}
	for k, v := range d.data {
		img.Items = append(img.Items, struct {
			FP           uint64 `json:"fp"`
			EpochUUID    string `json:"epoch_uuid"`
			EpochCounter uint64 `json:"epoch_counter"`
			Last         uint64 `json:"last"`
		}{FP: k.FP, EpochUUID: k.EpochUUID, EpochCounter: k.EpochCounter, Last: v})
	}
	b, _ := json.MarshalIndent(&img, "", "  ")
	return os.WriteFile(d.path, b, 0o644)
}

// shouldProcess applies dedupe rules and updates the store when accepting the event.
// Returns true if the event should be processed (and store updated), false otherwise.
func shouldProcess(fp uint64, epochUUID string, epochCounter, idx uint64, ds DedupeStore) bool {
	if ds == nil || idx == 0 {
		return false
	}
	last, ok := ds.Get(fp, epochUUID, epochCounter)
	if !ok {
		ds.Set(fp, epochUUID, epochCounter, idx)
		return true
	}
	if idx > last {
		ds.Set(fp, epochUUID, epochCounter, idx)
		return true
	}
	if idx == last {
		return false
	}
	// idx < last for the same epoch -> stale duplicate
	return false
}
