package ironhawk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dicedb/dicedb-go/wire"
	"google.golang.org/protobuf/encoding/protojson"
)

// AckPolicy defines when the client sends EMITACK
type AckPolicy int

const (
	AutoAckOnReceive AckPolicy = iota
	AutoAckAfterApply
	ManualAck
)

func (a AckPolicy) String() string {
	switch a {
	case AutoAckOnReceive:
		return "auto-on-receive"
	case AutoAckAfterApply:
		return "auto-after-apply"
	case ManualAck:
		return "manual"
	default:
		return "auto-on-receive"
	}
}

func ParseAckPolicy(s string) AckPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto-on-receive", "auto":
		return AutoAckOnReceive
	case "auto-after-apply", "after-apply":
		return AutoAckAfterApply
	case "manual":
		return ManualAck
	default:
		return AutoAckOnReceive
	}
}

// SubState represents the lifecycle of a subscription
type SubState int

const (
	StateActive SubState = iota
	StatePaused
	StateResuming
)

// Config controls emission/reconnect behavior
type Config struct {
	AckPolicy        AckPolicy
	AutoReconnect    bool
	AckBatchSize     int
	AckFlushInterval time.Duration
	Verbose          bool
}

// Subscription stores per-subscription state
type Subscription struct {
	SubID                    string
	Key                      string
	LastProcessedCommitIndex uint64
	ResumeNextIndex          uint64
	AckPolicy                AckPolicy
	State                    SubState

	// batching state
	mu           sync.Mutex
	highestIndex uint64
	pendingCount int
	flushTicker  *time.Ticker
}

// Manager handles active subscriptions (small wrapper for now)
type Manager struct {
	cfg Config
	sub *Subscription // current single watch in this CLI
	mu  sync.RWMutex
}

func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg}
}

func (m *Manager) SetCurrent(sub *Subscription) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sub = sub
}

func (m *Manager) Current() *Subscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sub
}

// extractCommitIndex attempts to find EmitSeq.CommitIndex in a watch Result using JSON as a generic path.
func extractCommitIndex(resp *wire.Result) (uint64, bool) {
	if resp == nil {
		return 0, false
	}
	b, err := protojson.Marshal(resp)
	if err != nil {
		return 0, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, false
	}
	// recursive search for keys "emitSeq" -> "commitIndex"
	var search func(any) (uint64, bool)
	search = func(v any) (uint64, bool) {
		switch t := v.(type) {
		case map[string]interface{}:
			// If we see commitIndex directly
			if ci, ok := t["commitIndex"]; ok {
				switch n := ci.(type) {
				case float64:
					return uint64(n), true
				case string:
					if u, err := strconv.ParseUint(n, 10, 64); err == nil {
						return u, true
					}
				}
			}
			// look deeper, prefer following an "emitSeq" child when available
			if es, ok := t["emitSeq"]; ok {
				if u, ok := search(es); ok {
					return u, true
				}
			}
			for _, v2 := range t {
				if u, ok := search(v2); ok {
					return u, true
				}
			}
		case []interface{}:
			for _, it := range t {
				if u, ok := search(it); ok {
					return u, true
				}
			}
		}
		return 0, false
	}
	return search(m)
}

// sendEmitAck sends EMITACK <key> <sub_id> <commit_index>
type fireClient interface {
	Fire(*wire.Command) *wire.Result
}

func sendEmitAck(client fireClient, key, subID string, commitIdx uint64) error {
	if client == nil {
		return errors.New("nil client")
	}
	cmd := &wire.Command{
		Cmd:  "EMITACK",
		Args: []string{key, subID, strconv.FormatUint(commitIdx, 10)},
	}
	res := client.Fire(cmd)
	if res == nil {
		return errors.New("no response from EMITACK")
	}
	if res.Status == wire.Status_ERR {
		return fmt.Errorf("EMITACK error: %s", res.Message)
	}
	return nil
}

// emitReconnect executes EMITRECONNECT and returns (status, nextIdx on OK)
func emitReconnect(client fireClient, key, subID string, lastIdx uint64) (string, uint64, error) {
	if client == nil {
		return "", 0, errors.New("nil client")
	}
	cmd := &wire.Command{
		Cmd:  "EMITRECONNECT",
		Args: []string{key, subID, strconv.FormatUint(lastIdx, 10)},
	}
	res := client.Fire(cmd)
	if res == nil {
		return "", 0, errors.New("no response from EMITRECONNECT")
	}
	if res.Status == wire.Status_ERR {
		// server encoded error in Message or typed payload
		return strings.ToUpper(strings.TrimSpace(res.Message)), 0, nil
	}
	// Expect res.Message like "OK <nextIndex>" or just "OK"
	msg := strings.TrimSpace(res.Message)
	up := strings.ToUpper(msg)
	if strings.HasPrefix(up, "OK") {
		parts := strings.Fields(msg)
		if len(parts) >= 2 {
			if u, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
				return "OK", u, nil
			}
		}
		return "OK", 0, nil
	}
	// Other textual statuses
	if up == "STALE_SEQUENCE" || up == "INVALID_SEQUENCE" || up == "SUBSCRIPTION_NOT_FOUND" {
		return up, 0, nil
	}
	// Unknown success case, fallback
	return msg, 0, nil
}

// maybeAckOnReceive applies ack policy for a single emission
func maybeAckOnReceive(mgr *Manager, client fireClient, sub *Subscription, resp *wire.Result) {
	if mgr == nil || sub == nil || resp == nil {
		return
	}
	// We call this after rendering (apply), so both AutoAckOnReceive and
	// AutoAckAfterApply are eligible here. ManualAck is skipped.
	if sub.AckPolicy == ManualAck {
		return
	}
	// Extract commit index
	if ci, ok := extractCommitIndex(resp); ok {
		// Resume filtering
		if sub.ResumeNextIndex > 0 && ci < sub.ResumeNextIndex {
			// ignore older entries defensively
			return
		}
		sub.mu.Lock()
		sub.LastProcessedCommitIndex = ci
		// batching
		if mgr.cfg.AckBatchSize > 0 || mgr.cfg.AckFlushInterval > 0 {
			if ci > sub.highestIndex {
				sub.highestIndex = ci
			}
			sub.pendingCount++
			shouldFlush := false
			if mgr.cfg.AckBatchSize > 0 && sub.pendingCount >= mgr.cfg.AckBatchSize {
				shouldFlush = true
			}
			if sub.flushTicker == nil && mgr.cfg.AckFlushInterval > 0 {
				sub.flushTicker = time.NewTicker(mgr.cfg.AckFlushInterval)
				go func(s *Subscription) {
					for range s.flushTicker.C {
						s.mu.Lock()
						hi := s.highestIndex
						pc := s.pendingCount
						s.highestIndex = 0
						s.pendingCount = 0
						s.mu.Unlock()
						if pc > 0 {
							_ = sendEmitAck(client, s.Key, s.SubID, hi)
						}
					}
				}(sub)
			}
			hi := sub.highestIndex
			pc := sub.pendingCount
			sub.mu.Unlock()
			if shouldFlush && pc > 0 {
				_ = sendEmitAck(client, sub.Key, sub.SubID, hi)
				sub.mu.Lock()
				sub.highestIndex = 0
				sub.pendingCount = 0
				sub.mu.Unlock()
			}
			return
		}
		sub.mu.Unlock()
		_ = sendEmitAck(client, sub.Key, sub.SubID, ci)
	}
}
