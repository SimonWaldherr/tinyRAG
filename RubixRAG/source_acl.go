package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// sourceACL is an optional, per-document access rule.  SourceAccess remains
// the coarse, source-kind rule; a source ACL can only narrow that rule.  The
// two identity dimensions deliberately match what an LDAP session already
// provides: the classified department and the person's mail/CN.
//
// An absent rule inherits SourceAccess.  A populated rule allows a caller
// when either its department or its user identity matches.  Administrators
// are always allowed, just as they are for SourceAccess.
type sourceACL struct {
	Departments []string `json:"departments,omitempty"`
	Users       []string `json:"users,omitempty"`
	UpdatedAt   int64    `json:"updated_at"`
}

type sourceACLStore struct {
	mu      sync.RWMutex
	path    string
	entries map[string]sourceACL
}

func newSourceACLStore(path string) (*sourceACLStore, error) {
	s := &sourceACLStore{path: path, entries: map[string]sourceACL{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read source ACLs: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.entries); err != nil {
		return nil, fmt.Errorf("parse source ACLs: %w", err)
	}
	for sourceID, rule := range s.entries {
		rule = normalizeSourceACL(rule)
		if sourceACLIsEmpty(rule) {
			delete(s.entries, sourceID)
			continue
		}
		s.entries[sourceID] = rule
	}
	return s, nil
}

func normalizeSourceACL(rule sourceACL) sourceACL {
	rule.Departments = normalizedACLValues(rule.Departments)
	rule.Users = normalizedACLValues(rule.Users)
	return rule
}

func normalizedACLValues(values []string) []string {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			seen[value] = true
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sourceACLIsEmpty(rule sourceACL) bool {
	return len(rule.Departments) == 0 && len(rule.Users) == 0
}

func (s *sourceACLStore) get(sourceID string) (sourceACL, bool) {
	if s == nil {
		return sourceACL{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rule, ok := s.entries[sourceID]
	return rule, ok
}

func (s *sourceACLStore) allowed(sourceID, deptCode, user string) bool {
	rule, ok := s.get(sourceID)
	if !ok || sourceACLIsEmpty(rule) || strings.EqualFold(deptCode, adminDeptCode) {
		return true
	}
	deptCode = strings.ToLower(strings.TrimSpace(deptCode))
	user = strings.ToLower(strings.TrimSpace(user))
	return aclValueAllowed(rule.Departments, deptCode) || aclValueAllowed(rule.Users, user)
}

func aclValueAllowed(allow []string, value string) bool {
	if value == "" {
		return false
	}
	for _, allowed := range allow {
		if allowed == "*" || allowed == value {
			return true
		}
	}
	return false
}

func (s *sourceACLStore) set(sourceID string, rule sourceACL) error {
	if s == nil {
		return fmt.Errorf("source ACL store is not initialized")
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return fmt.Errorf("missing source_id")
	}
	rule = normalizeSourceACL(rule)
	s.mu.Lock()
	if sourceACLIsEmpty(rule) {
		delete(s.entries, sourceID)
	} else {
		rule.UpdatedAt = time.Now().Unix()
		s.entries[sourceID] = rule
	}
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

func (s *sourceACLStore) delete(sourceID string) error {
	if s == nil || strings.TrimSpace(sourceID) == "" {
		return nil
	}
	s.mu.Lock()
	if _, ok := s.entries[sourceID]; !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.entries, sourceID)
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

func (s *sourceACLStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal source ACLs: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create source ACL directory: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write source ACLs: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace source ACLs: %w", err)
	}
	return nil
}
