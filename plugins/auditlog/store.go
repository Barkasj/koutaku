package auditlog

import (
	"fmt"
	"sync"
	"time"
)

// AuditLogStore is the persistence contract for audit events.
type AuditLogStore interface {
	// Append inserts a signed audit event.
	Append(event *AuditEvent) error
	// Query retrieves events matching the filter, ordered by timestamp descending.
	Query(filter QueryFilter) ([]AuditEvent, error)
	// Count returns the number of events matching the filter.
	Count(filter QueryFilter) (int64, error)
	// Purge removes events older than the given time.
	Purge(before time.Time) (int64, error)
}

// ---------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------

// MemoryStore is a thread-safe in-memory AuditLogStore for testing
// and single-instance deployments.
type MemoryStore struct {
	mu     sync.RWMutex
	events []AuditEvent
}

// NewMemoryStore creates an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

func (s *MemoryStore) Append(event *AuditEvent) error {
	if event == nil {
		return fmt.Errorf("auditlog: event cannot be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, *event)
	return nil
}

func (s *MemoryStore) Query(filter QueryFilter) ([]AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []AuditEvent
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		if !matchesFilter(e, filter) {
			continue
		}
		results = append(results, e)
		if filter.Limit > 0 && len(results) >= filter.Limit {
			break
		}
	}
	return results, nil
}

func (s *MemoryStore) Count(filter QueryFilter) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int64
	for _, e := range s.events {
		if matchesFilter(e, filter) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) Purge(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []AuditEvent
	var removed int64
	for _, e := range s.events {
		if e.Timestamp.Before(before) {
			removed++
		} else {
			kept = append(kept, e)
		}
	}
	s.events = kept
	return removed, nil
}

// matchesFilter returns true if the event satisfies every non-zero filter field.
func matchesFilter(e AuditEvent, f QueryFilter) bool {
	if f.Start != nil && e.Timestamp.Before(*f.Start) {
		return false
	}
	if f.End != nil && e.Timestamp.After(*f.End) {
		return false
	}
	if len(f.EventTypes) > 0 {
		found := false
		for _, et := range f.EventTypes {
			if e.EventType == et {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Severities) > 0 {
		found := false
		for _, s := range f.Severities {
			if e.Severity == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.ActorID != "" && e.Actor.ID != f.ActorID {
		return false
	}
	if f.ActorType != "" && e.Actor.Type != f.ActorType {
		return false
	}
	if f.Action != "" && e.Details.Action != f.Action {
		return false
	}
	return true
}
