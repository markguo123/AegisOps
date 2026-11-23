package sessionapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// sseFakeChat 是支持 SetSink/SetFatal 与展示读取的 ChatService 桩。
type sseFakeChat struct {
	emitter      func(func(sse.Event))
	fatal        func(string, sse.FatalData)
	session      *session.Session
	snap         sse.Snapshot
	events       []sse.Event
	snapshotHook func()
}

func (f *sseFakeChat) SetSink(sink func(sse.Event)) { f.emitter(sink) }
func (f *sseFakeChat) SetFatal(fn func(string, sse.FatalData)) {
	f.fatal = fn
}
func (f *sseFakeChat) Chat(_ context.Context, _ service.ChatRequest, _ <-chan string) (<-chan service.ChatEvent, error) {
	return make(chan service.ChatEvent), nil
}
func (f *sseFakeChat) GetOrCreateSession(id, _, _ string) (*session.Session, error) {
	return &session.Session{ID: id}, nil
}
func (f *sseFakeChat) GetSession(id string) (*session.Session, error) {
	if f.session != nil && f.session.ID == id {
		return f.session, nil
	}
	return &session.Session{ID: id}, nil
}
func (f *sseFakeChat) GetOrCreateSessionForUser(id, userID, _, _ string) (*session.Session, error) {
	return &session.Session{ID: id, UserID: userID}, nil
}
func (f *sseFakeChat) ListSessions() ([]session.Session, error)              { return nil, nil }
func (f *sseFakeChat) ListSessionsForUser(string) ([]session.Session, error) { return nil, nil }
func (f *sseFakeChat) DeleteSession(string) error                            { return nil }
func (f *sseFakeChat) RenameSession(string, string) error                    { return nil }
func (f *sseFakeChat) PinSession(string, bool) error                         { return nil }
func (f *sseFakeChat) Snapshot(_ context.Context, sessionID string, _ int, _ ...string) (sse.Snapshot, error) {
	if f.snapshotHook != nil {
		f.snapshotHook()
	}
	snap := f.snap
	snap.SessionID = sessionID
	return snap, nil
}
func (f *sseFakeChat) EventsAfter(_ context.Context, _ string, cursor uint64, limit int) ([]sse.Event, error) {
	var out []sse.Event
	for _, e := range f.events {
		if e.Seq > cursor && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *sseFakeChat) Head(_ context.Context, _ string) (uint64, uint64, error) {
	var head uint64
	for _, e := range f.events {
		if e.Seq > head {
			head = e.Seq
		}
	}
	return head, 0, nil
}

func principalRequest(t *testing.T, ctx context.Context, sessionID, userID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?protocol=2", nil)
	req.SetPathValue("id", sessionID)
	req = req.WithContext(withPrincipal(ctx, requestPrincipal{
		User: &store.User{ID: userID},
		Role: &store.Role{ID: "admin", Permissions: []string{"*"}},
	}))
	return req
}

// TestNewAttachesSink 覆盖提交后广播：sink 收到的事件以持久 SSE 帧扇出，
// 不再由 HTTP 层重新分配 seq。
func TestNewAttachesSink(t *testing.T) {
	var sink func(sse.Event)
	fake := &sseFakeChat{emitter: func(s func(sse.Event)) { sink = s }}
	srv := New(fake, "qwen-plus")
	if sink == nil {
		t.Fatal("sessionapi.New 未给服务装配提交后广播 sink")
	}

	b := srv.broadcaster("sse-web-1")
	ch := b.add()
	defer b.remove(ch)
	ev := sse.NewEvent("sse-web-1", "t1", sse.EventToolUpdated, sse.ToolUpdatedData{Tool: &sse.ToolCall{
		ToolCallID: "tc1", TurnID: "t1", Name: "kubectl", Status: sse.ToolRunning,
	}})
	ev.Seq = 7
	sink(ev)

	select {
	case frame := <-ch:
		got, err := sse.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("广播帧不是合法 SSE 事件: %v", err)
		}
		if got.SessionID != "sse-web-1" || got.EventType != sse.EventToolUpdated || got.TurnID != "t1" || got.Seq != 7 {
			t.Fatalf("广播事件 = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("广播未到达会话 SSE 客户端")
	}
}

func TestStreamRejectsOldProtocol(t *testing.T) {
	fake := &sseFakeChat{emitter: func(func(sse.Event)) {}, session: &session.Session{ID: "s1", UserID: "u1"}}
	srv := New(fake, "qwen-plus")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/s1/stream", nil)
	req.SetPathValue("id", "s1")
	req = req.WithContext(withPrincipal(context.Background(), requestPrincipal{
		User: &store.User{ID: "u1"}, Role: &store.Role{ID: "admin", Permissions: []string{"*"}},
	}))
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status=%d want 426, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UPGRADE_REQUIRED") {
		t.Fatalf("缺少升级提示: %s", rec.Body.String())
	}
}

func TestStreamSendsSnapshotThenReplay(t *testing.T) {
	msg := &sse.Message{
		MessageID: "m1", TurnID: "t1", InvocationID: "i1", Role: "assistant",
		Purpose: sse.PurposeFinal, Status: sse.MessageCompleted,
		Parts: []sse.Part{{PartID: "p1", Type: sse.PartText, Text: "全部正常", LastIndex: 1}},
	}
	e := sse.NewEvent("s1", "t1", sse.EventMessageCompleted, sse.MessageCompletedData{Message: msg})
	e.Seq = 5
	fake := &sseFakeChat{
		emitter: func(func(sse.Event)) {},
		session: &session.Session{ID: "s1", UserID: "u1"},
		snap: sse.Snapshot{ProtocolVersion: 2, SessionID: "s1", Cursor: 4,
			Turns: []sse.TurnView{{TurnID: "t1", SessionID: "s1", Status: sse.TurnRunning}}},
		events: []sse.Event{e},
	}
	srv := New(fake, "qwen-plus")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	srv.handleStream(rec, principalRequest(t, ctx, "s1", "u1"))
	body := rec.Body.String()
	if !strings.Contains(body, "event: stream.snapshot") {
		t.Fatalf("缺少规范快照帧: %s", body)
	}
	if !strings.Contains(body, "id: s1:5") || !strings.Contains(body, "event: message.completed") {
		t.Fatalf("缺少游标补齐事件: %s", body)
	}
}

func TestTimelineReturnsCanonicalSnapshot(t *testing.T) {
	fake := &sseFakeChat{
		emitter: func(func(sse.Event)) {},
		session: &session.Session{ID: "s1", UserID: "u1"},
		snap: sse.Snapshot{ProtocolVersion: 2, SessionID: "s1", Cursor: 9,
			Turns: []sse.TurnView{{TurnID: "t1", SessionID: "s1", Status: sse.TurnDone}}},
	}
	srv := New(fake, "qwen-plus")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/s1/timeline", nil)
	req.SetPathValue("id", "s1")
	req = req.WithContext(withPrincipal(context.Background(), requestPrincipal{
		User: &store.User{ID: "u1"}, Role: &store.Role{ID: "admin", Permissions: []string{"*"}},
	}))
	rec := httptest.NewRecorder()
	srv.handleTimeline(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got sse.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cursor != 9 || len(got.Turns) != 1 || got.Turns[0].Status != sse.TurnDone {
		t.Fatalf("timeline = %+v", got)
	}
}

// TestStreamFatalFrame 验证输出无法可靠保存时以非持久控制帧通知订阅者。
func TestStreamFatalFrame(t *testing.T) {
	fake := &sseFakeChat{emitter: func(func(sse.Event)) {}}
	srv := New(fake, "qwen-plus")
	if fake.fatal == nil {
		t.Fatal("sessionapi.New 未装配 fatal 钩子")
	}
	b := srv.broadcaster("s1")
	ch := b.add()
	defer b.remove(ch)
	fake.fatal("s1", sse.FatalData{Code: "PERSISTENCE_UNAVAILABLE", Message: "输出未能可靠保存", Retryable: true})
	select {
	case frame := <-ch:
		got, err := sse.DecodeControlFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if got.EventType != sse.FrameFatal {
			t.Fatalf("frame=%v", got.EventType)
		}
		data, _ := json.Marshal(got.Data)
		if !strings.Contains(string(data), "PERSISTENCE_UNAVAILABLE") {
			t.Fatalf("fatal data=%s", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fatal 帧未送达")
	}
}

func TestStreamForwardsFatalToBrowser(t *testing.T) {
	fake := &sseFakeChat{emitter: func(func(sse.Event)) {}, session: &session.Session{ID: "s1", UserID: "u1"}}
	srv := New(fake, "model")
	fake.snapshotHook = func() {
		fake.fatal("s1", sse.FatalData{Code: "PERSISTENCE_UNAVAILABLE", Message: "输出未保存", Retryable: true})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.handleStream(rec, principalRequest(t, ctx, "s1", "u1"))
	if !strings.Contains(rec.Body.String(), "event: stream.fatal") {
		t.Fatalf("fatal was swallowed: %s", rec.Body.String())
	}
	if ctx.Err() != nil {
		t.Fatal("fatal did not close the stream")
	}
}
