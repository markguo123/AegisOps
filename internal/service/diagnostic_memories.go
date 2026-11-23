package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/security"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

const confirmedDiagnosticMemory = "diagnostic-v1"

var (
	ErrMemoryDisabled = errors.New("diagnostic memory is disabled")
	ErrInvalidMemory  = errors.New("invalid diagnostic memory")
)

type DiagnosticMemory struct {
	SourceSessionID   string     `json:"sourceSessionId,omitempty"`
	SourceTurnID      string     `json:"sourceTurnId,omitempty"`
	RequestKey        string     `json:"requestKey,omitempty"`
	ExpiresAt         *time.Time `json:"expiresAt,omitempty"`
	Version           time.Time  `json:"version,omitempty"`
	ID                string     `json:"id,omitempty"`
	ClusterID         string     `json:"clusterId,omitempty"`
	Namespace         string     `json:"namespace,omitempty"`
	RelatedNamespaces []string   `json:"relatedNamespaces,omitempty"`
	Workload          string     `json:"workload,omitempty"`
	Text              string     `json:"text,omitempty"`
	Unavailable       bool       `json:"unavailable,omitempty"`
}

type DiagnosticMemories struct {
	chat   *chatService
	store  store.Store
	grants *authz.Service
}

func NewDiagnosticMemories(st store.Store, grants *authz.Service) *DiagnosticMemories {
	return &DiagnosticMemories{store: st, grants: grants}
}

func (s *DiagnosticMemories) enabled(ctx context.Context) (bool, error) {
	setting, err := s.store.Settings().Get(ctx, "memory", "enabled")
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return setting.Value == "true", nil
}

func (s *DiagnosticMemories) visible(ctx context.Context, owner string, m DiagnosticMemory) bool {
	if s.grants == nil || owner == "" || m.ClusterID == "" || m.Namespace == "" {
		return false
	}
	access, err := s.grants.EffectiveAccess(ctx, owner, m.ClusterID)
	if err != nil {
		return false
	}
	for _, ns := range append([]string{m.Namespace}, m.RelatedNamespaces...) {
		if access.Check(authz.Target{ClusterID: m.ClusterID, Namespace: ns, Actions: []string{authz.ActionRead}}) != nil {
			return false
		}
	}
	return true
}

func (s *DiagnosticMemories) Save(ctx context.Context, owner string, m DiagnosticMemory) (DiagnosticMemory, error) {
	m.ID = ""
	return s.save(ctx, owner, m)
}

func (s *DiagnosticMemories) Update(ctx context.Context, owner, id string, m DiagnosticMemory) (DiagnosticMemory, error) {
	m.ID = id
	return s.save(ctx, owner, m)
}

func (s *DiagnosticMemories) save(ctx context.Context, owner string, m DiagnosticMemory) (DiagnosticMemory, error) {
	on, err := s.enabled(ctx)
	if err != nil {
		return DiagnosticMemory{}, err
	}
	if !on {
		return DiagnosticMemory{}, ErrMemoryDisabled
	}
	m.Text = strings.TrimSpace(m.Text)
	m.Workload = strings.TrimSpace(m.Workload)
	if len(validation.IsDNS1123Subdomain(m.Workload)) != 0 || len(validation.IsDNS1123Label(m.Namespace)) != 0 ||
		(m.ExpiresAt != nil && !m.ExpiresAt.After(time.Now())) || len(m.RelatedNamespaces) > 10 || m.Text == "" || utf8.RuneCountInString(m.Text) > 500 ||
		security.NewRedactor().IsSensitive(m.Text) {
		return DiagnosticMemory{}, ErrInvalidMemory
	}
	for _, ns := range m.RelatedNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return DiagnosticMemory{}, ErrInvalidMemory
		}
	}
	if !s.visible(ctx, owner, m) {
		return DiagnosticMemory{}, store.ErrNotFound
	}
	requestKey := m.RequestKey
	m.RequestKey = ""
	if len(requestKey) > 128 {
		return DiagnosticMemory{}, ErrInvalidMemory
	}
	var existing *store.Memory
	if m.ID != "" {
		existing, err = s.store.Memories().Get(ctx, m.ID)
		if err != nil {
			return DiagnosticMemory{}, err
		}
		if existing.UserID != owner || existing.Scope != confirmedDiagnosticMemory {
			return DiagnosticMemory{}, store.ErrNotFound
		}
		var previous DiagnosticMemory
		if json.Unmarshal([]byte(existing.Content), &previous) != nil || !s.visible(ctx, owner, previous) {
			return DiagnosticMemory{}, store.ErrNotFound
		}
		if previous.ExpiresAt != nil && !previous.ExpiresAt.After(time.Now()) {
			return DiagnosticMemory{}, ErrInvalidMemory
		}
		if !existing.UpdatedAt.Equal(m.Version) {
			return DiagnosticMemory{}, store.ErrMemoryConflict
		}
	} else {
		if requestKey == "" {
			m.ID = "memory_" + rand.Text()
		} else {
			sum := sha256.Sum256([]byte(owner + "\x00" + requestKey))
			m.ID = fmt.Sprintf("memory_%x", sum[:16])
		}
	}
	if m.SourceSessionID != "" || m.SourceTurnID != "" {
		if !s.validSource(ctx, owner, m) {
			return DiagnosticMemory{}, store.ErrNotFound
		}
	}
	m.Version = time.Now().UTC().Truncate(time.Millisecond)
	if existing != nil && !m.Version.After(existing.UpdatedAt) {
		m.Version = existing.UpdatedAt.Add(time.Millisecond).Truncate(time.Millisecond)
	}
	m.Unavailable = false
	payload, err := json.Marshal(m)
	if err != nil {
		return DiagnosticMemory{}, err
	}
	row := &store.Memory{
		ID: m.ID, UserID: owner, ClusterID: m.ClusterID, Namespace: m.Namespace,
		Scope: confirmedDiagnosticMemory, Content: string(payload), UpdatedAt: m.Version,
	}
	if existing != nil {
		err = s.store.Memories().UpdateOwned(ctx, row, existing.Content)
	} else {
		err = s.store.Memories().CreateWithQuota(ctx, row)
		if err != nil && requestKey != "" {
			previous, lookupErr := s.store.Memories().Get(ctx, row.ID)
			if lookupErr == nil && previous.UserID == owner && previous.Scope == row.Scope {
				var saved DiagnosticMemory
				if json.Unmarshal([]byte(previous.Content), &saved) == nil {
					m.Version = saved.Version
					retry, marshalErr := json.Marshal(m)
					if marshalErr == nil && string(retry) == previous.Content {
						m.Version = previous.UpdatedAt
						return m, nil
					}
				}
				return DiagnosticMemory{}, store.ErrMemoryConflict
			}
		}
	}
	return m, err
}

func (s *DiagnosticMemories) List(ctx context.Context, owner, after string) ([]DiagnosticMemory, error) {
	rows, err := s.store.Memories().ListOwnedPage(ctx, owner, confirmedDiagnosticMemory, after, 50)
	if err != nil {
		return nil, err
	}
	result := make([]DiagnosticMemory, 0, len(rows))
	for _, row := range rows {
		if row.Scope != confirmedDiagnosticMemory {
			continue
		}
		var memory DiagnosticMemory
		if json.Unmarshal([]byte(row.Content), &memory) != nil || memory.ID != row.ID ||
			memory.ClusterID != row.ClusterID || memory.Namespace != row.Namespace || !s.visible(ctx, owner, memory) {
			memory = DiagnosticMemory{ID: row.ID, Unavailable: true}
		}
		if memory.SourceSessionID != "" && !s.validSource(ctx, owner, memory) {
			memory.SourceSessionID, memory.SourceTurnID = "", ""
		}
		memory.Version = row.UpdatedAt
		result = append(result, memory)
	}
	return result, nil
}

func (s *DiagnosticMemories) Forget(ctx context.Context, owner, id string) error {
	return s.store.Memories().DeleteOwned(ctx, owner, id)
}

var memoryQueryToken = regexp.MustCompile("[a-z0-9][a-z0-9.-]*")

// Recall resolves current data on each model request and checks both live grants and turn coverage.
func (s *DiagnosticMemories) Recall(ctx context.Context, owner, cluster, namespace, query string, check func(authz.Target) error) (string, []string) {
	on, err := s.enabled(ctx)
	if err != nil || !on || check == nil || cluster == "" {
		return "", nil
	}
	tokens := memoryQueryToken.FindAllString(strings.ToLower(query), -1)
	for i := range tokens {
		tokens[i] = strings.TrimRight(tokens[i], ".")
	}
	rows, err := s.store.Memories().ListOwnedForCluster(ctx, owner, cluster)
	if err != nil {
		return "", nil
	}
	var results []DiagnosticMemory
	for _, row := range rows {
		if row.Scope != confirmedDiagnosticMemory || row.ClusterID != cluster || (namespace != "" && row.Namespace != namespace) {
			continue
		}
		var memory DiagnosticMemory
		if json.Unmarshal([]byte(row.Content), &memory) != nil || memory.ID != row.ID ||
			memory.ClusterID != row.ClusterID || memory.Namespace != row.Namespace || !slices.Contains(tokens, memory.Workload) || (memory.ExpiresAt != nil && !memory.ExpiresAt.After(time.Now())) {
			continue
		}
		if !s.visible(ctx, owner, memory) || !memoryWithinCoverage(memory, check) {
			continue
		}
		if len(results) > 0 && (results[0].Namespace != memory.Namespace || results[0].Workload != memory.Workload) {
			return "", nil
		}
		results = append(results, memory)
	}
	if len(results) == 0 {
		return "", nil
	}
	var notes, ids []string
	var payload []byte
	for _, memory := range results[:min(len(results), 3)] {
		candidate := append(notes, memory.Text)
		encoded, err := json.Marshal(candidate)
		if err != nil || len(encoded) > 540 {
			continue
		}
		notes = candidate
		payload = encoded
		ids = append(ids, memory.ID)
	}
	return string(payload), ids
}

func memoryWithinCoverage(memory DiagnosticMemory, check func(authz.Target) error) bool {
	for _, ns := range append([]string{memory.Namespace}, memory.RelatedNamespaces...) {
		if check(authz.Target{ClusterID: memory.ClusterID, Namespace: ns, Actions: []string{authz.ActionRead}}) != nil {
			return false
		}
	}
	return true
}

func BindDiagnosticMemories(chat ChatService, memories *DiagnosticMemories) {
	if platformChat, ok := chat.(*chatService); ok {
		platformChat.diagnosticMemories = memories
		memories.chat = platformChat
	}
}

func (s *DiagnosticMemories) validSource(ctx context.Context, owner string, memory DiagnosticMemory) bool {
	if memory.SourceSessionID == "" || memory.SourceTurnID == "" {
		return false
	}
	source, err := s.store.Sessions().Get(ctx, memory.SourceSessionID)
	if err != nil || source.UserID != owner || source.Cluster != memory.ClusterID {
		return false
	}
	turn, err := s.store.Turns().Get(ctx, memory.SourceTurnID)
	return err == nil && turn.SessionID == source.ID && turn.UserID == owner && turn.Status == session.TurnDone
}
