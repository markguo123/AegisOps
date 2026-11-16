/**
 * 兼容导出层：Chat 流契约已统一到 `./chat`（协议 v2，方案 25）。
 * 旧 `AgentEvent`/`LegacyEvent`/`HistorySnapshot` 类型不再作为展示数据源。
 */
export {
  CHAT_PROTOCOL_VERSION,
  fetchChatStream,
  fetchTimeline,
  normalizeChatFrame,
  ChatStreamError,
} from './chat'
export type {
  ChatEvent,
  ChatEventType,
  ChatFrame,
  ChatInteraction,
  ChatItem,
  ChatNotice,
  ChatSnapshot,
  ChatTokenUsage,
  ChatToolCall,
  ChatTurn,
} from './chat'

/** 会话级 Token 用量别名（沿用既有调用点命名）。 */
export type AgentToken = import('./chat').ChatTokenUsage
