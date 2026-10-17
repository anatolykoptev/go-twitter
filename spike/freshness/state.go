package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile records everything cleanup needs to undo: which accounts were
// disabled for leasing (with their prior status), which users we followed or
// bell'd, and the list we created. It contains NO secrets — account IDs,
// statuses, X user IDs and the list ID only.
type stateFile struct {
	Phase       string            `json:"phase"` // setup | measuring | done | cleanup_done
	Started     time.Time         `json:"started"`
	Accounts    []accState        `json:"accounts"`
	ListID      string            `json:"list_id,omitempty"`
	ListMembers []string          `json:"list_member_ids,omitempty"`        // user IDs added to the list (arm B)
	Followed    []string          `json:"followed_user_ids,omitempty"`      // user IDs the probe followed (arm A)
	Belled      []string          `json:"belled_user_ids,omitempty"`        // user IDs the probe enabled bells on (arm D)
	Handles     map[string]string `json:"handles"`                          // resolved handle -> user ID
	SelfID      string            `json:"self_id,omitempty"`                // arm-A account's own user ID
	PreExisting []string          `json:"preexisting_follow_ids,omitempty"` // accounts A followed before setup
	Writes      int               `json:"writes"`                           // setup write count (undo ops excluded from the cap)
	// DevProbe records the single read-only device_follow probe result so a
	// re-run (or the full run after the dry run) never probes twice.
	DevProbe *devProbe `json:"dev_probe,omitempty"`

	mu sync.Mutex `json:"-"`
}

// devProbe is the outcome of the one read-only GET on device_follow.json.
type devProbe struct {
	Status int  `json:"status"`
	OK     bool `json:"ok"`
}

type accState struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	PriorStatus string `json:"prior_status"`
	Disabled    bool   `json:"disabled"`
}

func statePath(outDir string) string { return filepath.Join(outDir, "state.json") }

func loadState(outDir string) (*stateFile, error) {
	data, err := os.ReadFile(statePath(outDir))
	if errors.Is(err, os.ErrNotExist) {
		return &stateFile{Phase: "setup", Handles: map[string]string{}, Started: time.Now().UTC()}, nil
	}
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	if st.Handles == nil {
		st.Handles = map[string]string{}
	}
	return &st, nil
}

// saveState writes state.json atomically (tmp + rename) so a crash mid-write
// never leaves a torn state file behind.
func saveState(outDir string, st *stateFile) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(outDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(outDir))
}

// mutate runs fn under the state lock and persists the result. All mutation of
// a live stateFile goes through here — goroutines otherwise race on the map
// and slice fields.
func (s *stateFile) mutate(outDir string, fn func(*stateFile)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
	if err := saveState(outDir, s); err != nil {
		slog.Error("state save failed", "err", err)
	}
}

// snapshot returns the fields cleanup needs, read under the lock.
func (s *stateFile) snapshot() (belled, followed, members []string, listID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Belled...),
		append([]string(nil), s.Followed...),
		append([]string(nil), s.ListMembers...), s.ListID
}

// getListID is the lock-safe accessor the list-poller waits on.
func (s *stateFile) getListID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ListID
}
