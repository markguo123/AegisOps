package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestBranchSessionPersistsOnlyHistoryThroughCut(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "branch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := &chatService{store: db}
	source := &session.Session{
		ID: "source", UserID: "owner", Cluster: "test-cluster", ApprovedWrites: []string{"must-not-copy"},
		Messages: []session.Message{
			{Role: "tool", Content: "must-not-copy"},
			{Role: "tool", Name: "get_pod", Type: "tool-call-response", Content: "must-not-copy",
				PodObservation: &session.PodObservation{Cluster: "test-cluster", Namespace: "team", Name: "worker", TurnID: "cut"}},
			{Role: "tool", PodObservation: &session.PodObservation{Cluster: "test-cluster", Name: "later", TurnID: "later"}},
		},
	}
	if err := db.Sessions().Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index, id := range []string{"earlier", "cut", "later"} {
		turn := &session.Turn{
			ID: id, SessionID: source.ID, UserID: source.UserID, Status: session.TurnDone,
			Input: "Question " + id, Output: "Answer " + id, CreatedAt: now.Add(time.Duration(index) * time.Second),
		}
		if err := db.Turns().Create(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	recheck := func() error { return nil }
	if err := svc.BranchSession(ctx, source, "cut", "denied-branch", func() error { return session.ErrForbidden }); !errors.Is(err, session.ErrForbidden) {
		t.Fatalf("revoked grant must block branching: %v", err)
	}
	if _, err := db.Sessions().Get(ctx, "denied-branch"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked grant left a branch: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := svc.BranchSession(ctx, source, "cut", "child", recheck); err != nil {
			t.Fatal(err)
		}
	}
	child, err := db.Sessions().Get(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if child.Branch == nil || child.Branch.SessionID != "source" || child.Branch.TurnID != "cut" ||
		child.Cluster != source.Cluster || child.UserID != source.UserID || len(child.Messages) != 5 || len(child.ApprovedWrites) != 0 {
		t.Fatalf("inherited incorrect state: %+v", child)
	}
	if pod := child.Messages[3].PodObservation; pod == nil || pod.Name != "worker" || pod.TurnID != "child-t00000002" {
		t.Fatalf("unexpected inherited Pod observation: %+v", pod)
	}
	for _, message := range child.Messages {
		if strings.Contains(message.Content, "later") || strings.Contains(message.Content, "must-not-copy") {
			t.Fatalf("inherited unrelated history: %+v", message)
		}
	}
	snapshot, err := db.(store.ChatStore).ChatSnapshot(ctx, "child", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) != 2 || len(snapshot.Items) != 2 || snapshot.Items[1].Message.Purpose != sse.PurposeFinal ||
		snapshot.Items[1].Message.Text() != "Answer cut" {
		t.Fatalf("unexpected child replay: %+v", snapshot)
	}
	if err := db.Turns().Create(ctx, &session.Turn{ID: "pending", SessionID: source.ID, Status: session.TurnWaiting}); err != nil {
		t.Fatal(err)
	}
	if err := svc.BranchSession(ctx, source, "cut", "blocked", recheck); !errors.Is(err, ErrBranchSourceRunning) {
		t.Fatalf("pending operation must block branching: %v", err)
	}
}
