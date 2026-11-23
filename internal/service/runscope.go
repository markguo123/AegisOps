package service

import "sync"

// runScope 保存一次 Agent 运行的当前 turn/step 作用域（P1.9），供审计记录器
// 与 Skill 命中回调补全关联字段。展示事件的归属由 Agent 在源头绑定 ID，
// 不再依赖这里的“当前 step”推测。
type runScope struct {
	mu     sync.RWMutex
	sessID string
	turnID string
	stepID string
}

func (s *runScope) session() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessID
}

func (s *runScope) turn() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turnID
}

func (s *runScope) step() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stepID
}

func (s *runScope) turnStarted(turnID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.turnID = turnID
	s.stepID = ""
	s.mu.Unlock()
}

func (s *runScope) stepStarted(stepID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stepID = stepID
	s.mu.Unlock()
}
