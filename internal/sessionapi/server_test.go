package sessionapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// fakeChat 是 sessionapi 测试用 ChatService 桩。
type fakeChat struct {
	mu                 sync.Mutex
	sessions           map[string]*session.Session
	script             []service.ChatEvent
	gotChoice          string
	gotSkills          []string
	gotSkipPermissions bool
	// v2 展示事实源桩：发布后的事件与规范条目供 SSE/时间线读取。
	sink   func(sse.Event)
	seq    map[string]uint64
	events map[string][]sse.Event
	items  map[string][]sse.Item
	turns  map[string]sse.TurnView
}

func newFakeChat(script ...service.ChatEvent) *fakeChat {
	return &fakeChat{
		sessions: map[string]*session.Session{}, script: script,
		seq: map[string]uint64{}, events: map[string][]sse.Event{},
		items: map[string][]sse.Item{}, turns: map[string]sse.TurnView{},
	}
}

func (f *fakeChat) SetSink(sink func(sse.Event)) { f.sink = sink }

func (f *fakeChat) publish(sessionID, turnID string, typ sse.EventType, data any, item *sse.Item) {
	f.mu.Lock()
	f.seq[sessionID]++
	e := sse.NewEvent(sessionID, turnID, typ, data)
	e.Seq = f.seq[sessionID]
	f.events[sessionID] = append(f.events[sessionID], e)
	if item != nil {
		item.ItemOrder = int64(len(f.items[sessionID]) + 1)
		f.items[sessionID] = append(f.items[sessionID], *item)
	}
	if t, ok := f.turns[sessionID]; ok {
		t.Revision = e.Seq
		f.turns[sessionID] = t
	}
	sink := f.sink
	f.mu.Unlock()
	if sink != nil {
		sink(e)
	}
}

func (f *fakeChat) Snapshot(_ context.Context, sessionID string, _ int) (sse.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := sse.Snapshot{ProtocolVersion: 2, SessionID: sessionID, Cursor: f.seq[sessionID]}
	if t, ok := f.turns[sessionID]; ok {
		snap.Turns = append(snap.Turns, t)
	}
	snap.Items = append([]sse.Item(nil), f.items[sessionID]...)
	return snap, nil
}

func (f *fakeChat) EventsAfter(_ context.Context, sessionID string, cursor uint64, limit int) ([]sse.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sse.Event
	for _, e := range f.events[sessionID] {
		if e.Seq > cursor && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeChat) Head(_ context.Context, sessionID string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq[sessionID], 0, nil
}

func (f *fakeChat) GetOrCreateSession(id, _, _ string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		return s, nil
	}
	s := &session.Session{ID: id, Name: "Session " + id, UserID: "test-user"}
	f.sessions[id] = s
	return s, nil
}

func testPrincipal(_ context.Context, _ string) (*store.User, *store.Role, error) {
	return &store.User{ID: "test-user", Username: "tester"}, &store.Role{
		ID: "role-admin", Name: "admin", Permissions: []string{"*"},
	}, nil
}

func testHandler(s *Server) http.Handler {
	return s.AuthenticatedHandler(testPrincipal)
}

func (f *fakeChat) GetSession(id string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	return s, nil
}

func (f *fakeChat) GetOrCreateSessionForUser(id, userID, _, _ string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		if s.UserID != userID {
			return nil, session.ErrForbidden
		}
		return s, nil
	}
	s := &session.Session{ID: id, Name: "Session " + id, UserID: userID}
	f.sessions[id] = s
	return s, nil
}

func (f *fakeChat) ListSessions() ([]session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]session.Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeChat) ListSessionsForUser(userID string) ([]session.Session, error) {
	all, err := f.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(all))
	for _, sess := range all {
		if sess.UserID == userID {
			out = append(out, sess)
		}
	}
	return out, nil
}

func (f *fakeChat) DeleteSession(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	return nil
}

func (f *fakeChat) RenameSession(id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		s.Name = name
	}
	return nil
}

func (f *fakeChat) PinSession(id string, pinned bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		s.Pinned = pinned
	}
	return nil
}

func (f *fakeChat) Chat(ctx context.Context, req service.ChatRequest, respond <-chan string) (<-chan service.ChatEvent, error) {
	f.mu.Lock()
	f.gotSkills = append([]string{}, req.Skills...)
	f.gotSkipPermissions = req.SkipPermissions
	f.mu.Unlock()
	turnID := req.TurnID
	if turnID == "" {
		turnID = "turn-test"
	}
	out := make(chan service.ChatEvent, len(f.script)+4)
	finish := func(status sse.TurnStatus) {
		f.mu.Lock()
		t := f.turns[req.SessionID]
		t.TurnID, t.SessionID, t.Status = turnID, req.SessionID, status
		if t.Usage.TotalTokens == 0 {
			t.Usage = sse.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
		}
		f.turns[req.SessionID] = t
		f.mu.Unlock()
	}
	go func() {
		defer close(out)
		f.mu.Lock()
		f.turns[req.SessionID] = sse.TurnView{TurnID: turnID, SessionID: req.SessionID, Input: req.Query, Status: sse.TurnRunning}
		f.mu.Unlock()
		f.publish(req.SessionID, turnID, sse.EventTurnStatus, sse.TurnStatusData{TurnID: turnID, Status: sse.TurnRunning}, nil)
		toolSeen := map[string]*sse.ToolCall{}
		for _, e := range f.script {
			if e.Type == service.ChatEventToolResponse && e.Tool != nil {
				f.persistToolResponse(req.SessionID, e.Tool)
				tool := toolSeen[e.Tool.Name]
				if tool == nil {
					tool = &sse.ToolCall{ToolCallID: "tc-" + e.Tool.Name, TurnID: turnID, Name: e.Tool.Name}
				}
				tool.Status = sse.ToolSucceeded
				tool.Result = e.Tool.Result
				tool.Summary = e.Tool.Summary
				tool.Data = e.Tool.Data
				f.publish(req.SessionID, turnID, sse.EventToolUpdated, sse.ToolUpdatedData{Tool: tool},
					&sse.Item{Type: sse.ItemTool, Tool: tool})
			}
			if e.Type == service.ChatEventChoice {
				interaction := &sse.Interaction{
					InteractionID: "int-test", TurnID: turnID, Kind: sse.InteractionApproval,
					Status: sse.InteractionPending, Prompt: "确认执行？",
					Options: []sse.ChoiceOption{{Label: "确认执行", Value: "yes"}},
				}
				if e.Choice != nil && e.Choice.Approval != nil {
					interaction.InteractionID = e.Choice.Approval.InteractionID
					if interaction.InteractionID == "" {
						interaction.InteractionID = "int-test"
					}
					interaction.Prompt = e.Choice.Prompt
				}
				f.publish(req.SessionID, turnID, sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: interaction},
					&sse.Item{Type: sse.ItemInteraction, Interaction: interaction})
				select {
				case <-ctx.Done():
					return
				case c := <-respond:
					f.mu.Lock()
					f.gotChoice = c
					f.mu.Unlock()
					interaction.Status = sse.InteractionApproved
					f.publish(req.SessionID, turnID, sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: interaction},
						&sse.Item{Type: sse.ItemInteraction, Interaction: interaction})
					answer := "approved:" + c
					msg := &sse.Message{
						MessageID: "m-answer", TurnID: turnID, InvocationID: "i-answer", Role: "assistant",
						Purpose: sse.PurposeFinal, Status: sse.MessageCompleted,
						Parts: []sse.Part{{PartID: "p-answer", Type: sse.PartText, Text: answer, LastIndex: 1}},
					}
					f.publish(req.SessionID, turnID, sse.EventMessageCompleted, sse.MessageCompletedData{Message: msg},
						&sse.Item{Type: sse.ItemMessage, Message: msg})
					finish(sse.TurnDone)
					f.publish(req.SessionID, turnID, sse.EventTurnStatus, sse.TurnStatusData{TurnID: turnID, Status: sse.TurnDone, FinalMessageID: msg.MessageID}, nil)
					out <- service.ChatEvent{Type: service.ChatEventAnswer, Text: answer}
					return
				}
			}
			if e.Type == service.ChatEventToolCall && e.Tool != nil {
				tool := &sse.ToolCall{ToolCallID: "tc-" + e.Tool.Name, TurnID: turnID, Name: e.Tool.Name, Status: sse.ToolRunning}
				toolSeen[e.Tool.Name] = tool
				f.publish(req.SessionID, turnID, sse.EventToolUpdated, sse.ToolUpdatedData{Tool: tool},
					&sse.Item{Type: sse.ItemTool, Tool: tool})
			}
			if e.Type == service.ChatEventAnswer && e.Text != "" {
				msg := &sse.Message{
					MessageID: "m-answer", TurnID: turnID, InvocationID: "i-answer", Role: "assistant",
					Purpose: sse.PurposeFinal, Status: sse.MessageCompleted,
					Parts: []sse.Part{{PartID: "p-answer", Type: sse.PartText, Text: e.Text, LastIndex: 1}},
				}
				f.publish(req.SessionID, turnID, sse.EventMessageCompleted, sse.MessageCompletedData{Message: msg},
					&sse.Item{Type: sse.ItemMessage, Message: msg})
			}
			out <- e
		}
		finish(sse.TurnDone)
		f.publish(req.SessionID, turnID, sse.EventTurnStatus, sse.TurnStatusData{TurnID: turnID, Status: sse.TurnDone}, nil)
	}()
	return out, nil
}

func TestWebSendMessageCannotEnableSkipPermissions(t *testing.T) {
	chat := newFakeChat()
	srv := httptest.NewServer(testHandler(New(chat, "qwen-plus")))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/sessions", `{"id":"s-skip"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session status = %d", resp.StatusCode)
	}

	resp = postJSON(t, srv.URL+"/api/v1/sessions/s-skip/send-message", `{"query":"修复","skipPermissions":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	chat.mu.Lock()
	got := chat.gotSkipPermissions
	chat.mu.Unlock()
	if got {
		t.Fatal("Web request body must not enable skipPermissions")
	}
}

func (f *fakeChat) persistToolResponse(id string, tool *service.ChatToolCallView) {
	if id == "" || tool == nil {
		return
	}
	var args json.RawMessage
	if b, err := json.Marshal(tool.Arguments); err == nil {
		args = b
	}
	ok := false
	if tool.Ok != nil {
		ok = *tool.Ok
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, found := f.sessions[id]
	if !found {
		return
	}
	sess.AddMessage(session.Message{Role: "tool", Type: "tool-call-response", Name: tool.Name, Arguments: args, Data: tool.Data, Ok: &ok, Content: tool.Result})
}

func postJSON(t *testing.T, url string, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type cancelChat struct {
	*fakeChat
	started  chan struct{}
	canceled chan struct{}
}

func (c *cancelChat) Chat(ctx context.Context, _ service.ChatRequest, _ <-chan string) (<-chan service.ChatEvent, error) {
	out := make(chan service.ChatEvent, 4)
	go func() {
		close(c.started)
		<-ctx.Done()
		close(c.canceled)
		close(out)
	}()
	return out, nil
}

func TestSessionCRUD(t *testing.T) {
	srv := httptest.NewServer(testHandler(New(newFakeChat(), "qwen-plus")))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/sessions", `{"id":"s1","name":"incident-001"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/api/v1/sessions")
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["name"] != "incident-001" {
		t.Fatalf("sessions = %v", list)
	}

	resp = postJSON(t, srv.URL+"/api/v1/sessions/s1/rename", `{"name":"renamed"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/s1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
}

func TestCancelActiveRun(t *testing.T) {
	fake := &cancelChat{
		fakeChat: newFakeChat(),
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	_, _ = fake.GetOrCreateSession("s1", "", "m")
	srv := httptest.NewServer(testHandler(New(fake, "m")))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/sessions/s1/send-message", `{"query":"检查"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case <-fake.started:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
	}

	resp = postJSON(t, srv.URL+"/api/v1/sessions/s1/cancel", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case <-fake.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("context was not canceled")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp = postJSON(t, srv.URL+"/api/v1/sessions/s1/cancel", `{}`)
		if resp.StatusCode == http.StatusConflict {
			resp.Body.Close()
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("active run cleanup did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startSSE 打开一条协议 v2 SSE 连接：收到规范快照后通知 connected，继续读到 want。
// 必须在独立 goroutine 中调用（阻塞式）。
func startSSE(url string, connected chan<- struct{}, want string) (string, error) {
	if !strings.Contains(url, "?") {
		url += "?protocol=2"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		switch {
		case strings.Contains(data, "stream.snapshot"):
			close(connected)
		case strings.Contains(data, want):
			return data, nil
		}
	}
	return "", sc.Err()
}

func readHistory(url string) (string, error) {
	if !strings.Contains(url, "?") {
		url += "?protocol=2"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.Contains(data, "stream.snapshot") {
			return data, nil
		}
	}
	return "", sc.Err()
}

type sseResult struct {
	data string
	err  error
}

func TestStreamAndSendMessage(t *testing.T) {
	fake := newFakeChat(
		service.ChatEvent{Type: service.ChatEventToolCall, Tool: &service.ChatToolCallView{Name: "get_resources"}},
		service.ChatEvent{Type: service.ChatEventAnswer, Text: "完成"},
	)
	srv := httptest.NewServer(testHandler(New(fake, "m")))
	defer srv.Close()
	_, _ = fake.GetOrCreateSession("s1", "", "m")

	connected := make(chan struct{})
	res := make(chan sseResult, 1)
	go func() {
		data, err := startSSE(srv.URL+"/api/v1/sessions/s1/stream", connected, "完成")
		res <- sseResult{data: data, err: err}
	}()
	<-connected

	resp := postJSON(t, srv.URL+"/api/v1/sessions/s1/send-message", `{"query":"检查"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !strings.Contains(r.data, "message.completed") && !strings.Contains(r.data, "完成") {
			t.Fatalf("SSE 数据异常: %s", r.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 answer 事件")
	}
}

func TestSendMessageForwardsExplicitSkills(t *testing.T) {
	fake := newFakeChat(service.ChatEvent{Type: service.ChatEventAnswer, Text: "完成"})
	srv := httptest.NewServer(testHandler(New(fake, "m")))
	defer srv.Close()
	_, _ = fake.GetOrCreateSession("s1", "", "m")

	resp := postJSON(t, srv.URL+"/api/v1/sessions/s1/send-message", `{"query":"检查 mysql 故障","skills":["k8s-mysql-troubleshoot"]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.gotSkills) != 1 || fake.gotSkills[0] != "k8s-mysql-troubleshoot" {
		t.Fatalf("skills 未转发到 ChatRequest：%v", fake.gotSkills)
	}
}

func TestCreateResourceHistoryRecovery(t *testing.T) {
	dryData := map[string]any{
		"mode":             "dry_run",
		"status":           "validated",
		"proposalId":       "prop-1",
		"proposalManifest": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: web\ntype: Opaque\nstringData:\n  password: <generated>",
		"objects": []any{
			map[string]any{
				"kind": "Secret", "name": "db", "namespace": "web", "status": "validated",
				"secretKeys": []any{map[string]any{"key": "password", "length": 16, "sha256": "abc123"}},
			},
		},
	}
	appData := map[string]any{
		"mode": "apply", "status": "applied", "applied": 1, "refused": 0,
		"objects": []any{
			map[string]any{
				"kind": "Secret", "name": "db", "namespace": "web", "status": "applied", "uid": "uid-1",
				"verify": map[string]any{"ready": true, "reason": "created ok"},
			},
		},
		"secretTouches": []any{
			map[string]any{"kind": "Secret", "namespace": "web", "name": "db", "keys": []any{map[string]any{"key": "password", "length": 16, "sha256": "abc123"}}},
		},
	}
	approveOK := true
	script := []service.ChatEvent{
		{Type: service.ChatEventToolCall, Tool: &service.ChatToolCallView{Name: "create_resource", Arguments: map[string]any{"mode": "dry_run", "manifest": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db"}}},
		{Type: service.ChatEventToolResponse, Tool: &service.ChatToolCallView{Name: "create_resource", Result: "dry run ok", Ok: &approveOK, Data: dryData}},
		{Type: service.ChatEventToolCall, Tool: &service.ChatToolCallView{Name: "create_resource", Arguments: map[string]any{"mode": "apply", "proposal_id": "prop-1"}}},
		{Type: service.ChatEventToolResponse, Tool: &service.ChatToolCallView{Name: "create_resource", Result: "created", Ok: &approveOK, Data: appData}},
		{Type: service.ChatEventAnswer, Text: "完成"},
	}
	fake := newFakeChat(script...)
	srv := httptest.NewServer(testHandler(New(fake, "m")))
	defer srv.Close()
	_, _ = fake.GetOrCreateSession("s1", "", "m")

	connected := make(chan struct{})
	res := make(chan sseResult, 1)
	go func() {
		data, err := startSSE(srv.URL+"/api/v1/sessions/s1/stream", connected, "完成")
		res <- sseResult{data: data, err: err}
	}()
	<-connected
	resp := postJSON(t, srv.URL+"/api/v1/sessions/s1/send-message", `{"query":"创建"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到完成事件")
	}

	history, err := readHistory(srv.URL + "/api/v1/sessions/s1/stream")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history, "create_resource") || !strings.Contains(history, "proposalManifest") {
		t.Fatalf("会话历史未恢复创建提案: %s", history)
	}
	if !strings.Contains(history, "applied") || !strings.Contains(history, "ready") {
		t.Fatalf("会话历史未恢复创建结果: %s", history)
	}
	if !strings.Contains(history, "sha256") {
		t.Fatalf("会话历史应包含 Secret 指纹: %s", history)
	}
	if strings.Contains(history, "Gientech@123") {
		t.Fatalf("会话历史不该包含 Secret 明文值: %s", history)
	}
}

func TestApproveFlow(t *testing.T) {
	fake := newFakeChat(service.ChatEvent{Type: service.ChatEventChoice, Choice: &service.ChatChoice{Prompt: "是否执行？"}})
	srv := httptest.NewServer(testHandler(New(fake, "m")))
	defer srv.Close()
	_, _ = fake.GetOrCreateSession("s1", "", "m")

	connected := make(chan struct{})
	res := make(chan sseResult, 1)
	go func() {
		data, err := startSSE(srv.URL+"/api/v1/sessions/s1/stream", connected, "approved:")
		res <- sseResult{data: data, err: err}
	}()
	<-connected

	resp := postJSON(t, srv.URL+"/api/v1/sessions/s1/send-message", `{"query":"扩容"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/api/v1/sessions/s1/approve", `{"choice":"yes"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !strings.Contains(r.data, "approved:yes") {
			t.Fatalf("SSE 数据异常: %s", r.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到审批结果事件")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.gotChoice != "yes" {
		t.Fatalf("choice = %q", fake.gotChoice)
	}
}
