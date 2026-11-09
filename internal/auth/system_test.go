package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSystemGuardBoundsConsoleSessionsAndReclaimsExpiredEntries(t *testing.T) {
	now := time.Now()
	guard := &SystemGuard{sessions: make(map[string]*ConsoleSession, MaxConsoleSessions)}
	for index := 0; index < MaxConsoleSessions; index++ {
		guard.sessions[fmt.Sprintf("session-%d", index)] = &ConsoleSession{Email: "admin@example.test", ExpiresAt: now.Add(time.Hour)}
	}
	if _, err := guard.CreateSessionBounded("admin@example.test"); !errors.Is(err, ErrConsoleSessionCapacity) {
		t.Fatalf("full session store error = %v, want capacity exhausted", err)
	}
	if len(guard.sessions) != MaxConsoleSessions {
		t.Fatalf("full session count = %d, want %d", len(guard.sessions), MaxConsoleSessions)
	}
	guard.sessions["expired"] = &ConsoleSession{Email: "admin@example.test", ExpiresAt: now.Add(-time.Second)}
	delete(guard.sessions, "session-0")
	if _, err := guard.CreateSessionBounded("admin@example.test"); err != nil {
		t.Fatalf("create after expired-session reclamation: %v", err)
	}
	if len(guard.sessions) != MaxConsoleSessions {
		t.Fatalf("session count after expired reclamation = %d, want %d", len(guard.sessions), MaxConsoleSessions)
	}
}

func TestSystemGuardCloseCancelsAndJoinsCleanup(t *testing.T) {
	guard := &SystemGuard{sessions: make(map[string]*ConsoleSession)}
	guard.startCleanup()
	done := guard.cleanupDone
	if err := guard.Close(); err != nil {
		t.Fatal("close system guard:", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("system guard cleanup worker did not exit")
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestPausedSystemGuardDefersCleanupUntilStartAndCloseWinsRace(t *testing.T) {
	guard := &SystemGuard{sessions: make(map[string]*ConsoleSession)}
	if guard.cleanupDone != nil {
		t.Fatal("paused guard started cleanup during construction")
	}
	guard.StartCleanup()
	done := guard.cleanupDone
	if done == nil {
		t.Fatal("StartCleanup did not create worker completion signal")
	}
	if err := guard.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	guard.StartCleanup()
	select {
	case <-done:
	default:
		t.Fatal("cleanup did not join or restarted after Close")
	}
}
