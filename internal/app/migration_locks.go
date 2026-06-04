package app

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type migrationLock struct {
	LeaseID   string    `json:"leaseId"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type migrationLockStore struct {
	mu    sync.Mutex
	locks map[string]migrationLock
}

func newMigrationLockStore() *migrationLockStore {
	return &migrationLockStore{locks: make(map[string]migrationLock)}
}

func (s *migrationLockStore) acquire(key, owner string, ttl time.Duration) (migrationLock, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now())
	existing, ok := s.locks[key]
	if ok {
		return existing, existing.Owner == owner
	}
	lock := migrationLock{
		LeaseID:   randomLeaseID(),
		Owner:     owner,
		ExpiresAt: time.Now().Add(ttl).UTC(),
	}
	s.locks[key] = lock
	return lock, true
}

func (s *migrationLockStore) get(key string) (migrationLock, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now())
	lock, ok := s.locks[key]
	return lock, ok
}

func (s *migrationLockStore) release(key, leaseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[key]
	if !ok || lock.LeaseID != leaseID {
		return false
	}
	delete(s.locks, key)
	return true
}

func (s *migrationLockStore) pruneExpiredLocked(now time.Time) {
	for key, lock := range s.locks {
		if now.After(lock.ExpiresAt) {
			delete(s.locks, key)
		}
	}
}

func randomLeaseID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(bytes[:])
}
