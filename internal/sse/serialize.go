package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// MarshalEvent 返回 v2 事件的 JSON 编码（服务端广播负载，幂等可复用）。
func MarshalEvent(e Event) ([]byte, error) {
	return json.Marshal(e)
}

// MarshalControl 返回连接控制帧的 JSON 编码。
func MarshalControl(f ControlFrame) ([]byte, error) {
	return json.Marshal(f)
}

// EventFrame 把语义事件序列化为完整 SSE 帧。持久事件带 `id: <session>:<seq>`
// （供 Last-Event-ID 续传）；控制帧不带 id，也不推进游标。
func EventFrame(e Event) []byte {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	if e.Seq > 0 && e.SessionID != "" {
		b.WriteString("id: ")
		b.WriteString(SSEID(e.SessionID, e.Seq))
		b.WriteString("\n")
	}
	b.WriteString("event: ")
	b.WriteString(string(e.EventType))
	b.WriteString("\n")
	writeDataLines(&b, raw)
	b.WriteString("\n")
	return b.Bytes()
}

// ControlFrameBytes 把控制帧序列化为完整 SSE 帧（无 id）。
func ControlFrameBytes(f ControlFrame) []byte {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(string(f.EventType))
	b.WriteString("\n")
	writeDataLines(&b, raw)
	b.WriteString("\n")
	return b.Bytes()
}

// Heartbeat 返回注释心跳帧：不推进游标、不占业务 seq。
func Heartbeat() []byte { return []byte(": keep-alive\n\n") }

func writeDataLines(b *bytes.Buffer, raw []byte) {
	for _, line := range strings.Split(string(raw), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// Decode 从裸 JSON 解析 v2 事件。
func Decode(b []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	return e, nil
}

// DecodeFrame 从完整 SSE 帧（或裸 JSON）解析 v2 事件，供测试与诊断复用。
func DecodeFrame(b []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(b, &e); err == nil && e.EventType != "" {
		return e, nil
	}
	raw, err := dataOfFrame(b)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	return e, nil
}

// DecodeControlFrame 从完整 SSE 帧（或裸 JSON）解析连接控制帧。
func DecodeControlFrame(b []byte) (ControlFrame, error) {
	var f ControlFrame
	if err := json.Unmarshal(b, &f); err == nil && f.EventType != "" {
		return f, nil
	}
	raw, err := dataOfFrame(b)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	return f, nil
}

func dataOfFrame(b []byte) ([]byte, error) {
	var dataLines []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return nil, errors.New("sse: no data line in frame")
	}
	return []byte(strings.Join(dataLines, "\n")), nil
}
