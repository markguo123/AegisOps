<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { List } from '@element-plus/icons-vue'

import type {
  ChatInteraction,
  ChatItem,
  ChatMessage,
  ChatNotice,
  ChatToolCall,
  ChatTurn,
} from '@/api/chat'
import type { ApprovalUiData } from './ApprovalCard.vue'
import type { ToolUiData } from './ToolCallCard.vue'
import type { NoticeUiData, QuestionUiData, ReportUiData, TurnItem, TurnUiData, TurnStatus } from './turn'
import TechEmptyState from '@/components/TechEmptyState.vue'
import TurnBlock from './TurnBlock.vue'
import { parseJsonLoose } from './render/constants'

/**
 * 统一时间线（方案 25 §10）：只消费 reducer 里的规范 Turn/Item，
 * 历史快照与实时事件同 ID、同结构；状态只由 Turn 状态与真实 activity 驱动，
 * 不按卡片推测阶段，不造“思考→工具→观察→报告”四步。
 */
const props = withDefaults(
  defineProps<{
    turns: ChatTurn[]
    canBranch?: boolean
    items: ChatItem[]
    /** 当前用户是否有权审批：false 时审批卡收起操作区（后端仍强校验）。 */
    canDecide?: boolean
    emptyTitle?: string
    emptyHint?: string
    /** 当前会话绑定的集群名称：审批卡展示“集群 + 资源目标”。 */
    clusterName?: string
  }>(),
  {
    canDecide: true,
    canBranch: false,
    clusterName: '',
    emptyTitle: '选择或新建一个会话',
    emptyHint: '提问后助手会先检查现场、再给出可复核的结论',
  },
)

const emit = defineEmits<{
  (e: 'branch', turnId: string): void
  (e: 'approve', value: string, id?: string): void
  (e: 'reject', id?: string): void
  (
    e: 'answer',
    payload: { interactionId: string; status: 'selected' | 'text' | 'skipped'; optionId?: string; text?: string },
  ): void
}>()

const rollRef = ref<HTMLElement | null>(null)

function toolStatus(tool: ChatToolCall): ToolUiData['status'] {
  switch (tool.status) {
    case 'succeeded':
      return 'done'
    case 'failed':
    case 'denied':
      return 'error'
    case 'unknown':
      return 'unknown'
    default:
      return 'running'
  }
}

function summarizeCommand(tool: string, params: unknown): string {
  if (typeof params === 'string') {
    const trimmed = params.trim()
    if (/^\s*kubectl\b/.test(trimmed) || /\s/.test(trimmed)) {
      return trimmed.slice(0, 160)
    }
    const record = parseJsonLoose(trimmed)
    if (record !== trimmed && record && typeof record === 'object') {
      return summarizeRecord(tool, record as Record<string, unknown>)
    }
    return ''
  }
  if (params && typeof params === 'object' && !Array.isArray(params)) {
    return summarizeRecord(tool, params as Record<string, unknown>)
  }
  return ''
}

function summarizeRecord(tool: string, record: Record<string, unknown>): string {
  for (const key of ['command', 'cmd', 'exec']) {
    const value = record[key]
    if (typeof value === 'string' && value) {
      return value.slice(0, 160)
    }
  }
  const argv =
    (Array.isArray(record['argv']) && record['argv']) ||
    (Array.isArray(record['args']) && record['args'])
  if (argv) {
    return (argv as unknown[]).map(String).join(' ').slice(0, 160)
  }
  if (typeof record['namespace'] === 'string' && record['namespace']) {
    return `${tool} --namespace ${record['namespace']}`
  }
  return ''
}

function shortNameOf(tool: string, command: string): string {
  const match = command.trim().match(/\b(?:kubectl|helm|aopsctl|kubescape)\s+([\w-]+)/i)
  if (match) {
    return match[1]
  }
  if (command.trim()) {
    return command.trim().split(/\s+/)[0].slice(0, 12)
  }
  const parts = tool.split(/[.\-_:]/).filter(Boolean)
  return (parts[parts.length - 1] || tool).slice(0, 12)
}

function detectCreateResource(name: string, params: unknown, ok: boolean): ToolUiData['createResource'] {
  if (!/create_resource|create-resource/.test(name)) {
    return null
  }
  const record = params && typeof params === 'object' ? (params as Record<string, unknown>) : {}
  const manifest =
    (typeof record.manifest === 'string' && record.manifest) ||
    (typeof record.command === 'string' && record.command) ||
    ''
  const status = typeof record.status === 'string' ? record.status : ''
  return {
    manifest,
    dryRun: /dry.?run/i.test(status) || manifest.length > 0,
    validated: ok || /validated|ready/i.test(status),
    applied: ok || /applied/.test(status),
  }
}

function toolUi(tool: ChatToolCall): ToolUiData {
  const parsed = tool.params ? parseJsonLoose(tool.params) : null
  const params = parsed !== null && parsed !== tool.params ? parsed : tool.params
  const command = tool.command || summarizeCommand(tool.name, params)
  const status = toolStatus(tool)
  const ok = tool.ok ?? status === 'done'
  return {
    key: tool.tool_call_id,
    name: tool.name,
    shortName: shortNameOf(tool.name, command),
    denied: tool.status === 'denied',
    command,
    risk: tool.risk ?? '',
    status,
    ok,
    params,
    result: tool.data ?? tool.result ?? null,
    raw: tool.result ?? (typeof params === 'string' ? params : ''),
    duration: typeof tool.duration_ms === 'number' ? tool.duration_ms : null,
    summary: tool.summary,
    displayType: tool.display_type,
    error: tool.error,
    errorCode: tool.error_code,
    stdout: tool.stdout,
    stderr: tool.stderr,
    exitCode: tool.exit_code,
    createResource: detectCreateResource(tool.name, params, ok),
  }
}

function approvalUi(interaction: ChatInteraction): ApprovalUiData {
  const decision: ApprovalUiData['decision'] =
    interaction.status === 'approved'
      ? 'approved'
      : interaction.status === 'rejected'
        ? 'rejected'
        : interaction.status === 'pending'
          ? 'pending'
          : 'rejected'
  return {
    id: interaction.interaction_id,
    key: interaction.interaction_id,
    title: interaction.title,
    operation: interaction.operation,
    toolCallId: interaction.tool_call_id,
    command: interaction.command,
    prompt: interaction.prompt ?? '需要你的审批确认',
    options: (interaction.options ?? []).map((option) => ({ label: option.label, value: option.value })),
    secret: interaction.secret === true || interaction.kind === 'secret_read',
    decision,
    tool: interaction.tool ?? '',
    risk: interaction.risk ?? '',
    summary: interaction.summary ?? '',
    resource: null,
    target: interaction.target,
    changes: interaction.changes,
    checks: interaction.checks,
    verification: interaction.verification,
  }
}

function questionUi(interaction: ChatInteraction, sessionId: string): QuestionUiData {
  return {
    key: interaction.interaction_id,
    interactionId: interaction.interaction_id,
    sessionId,
    turnId: interaction.turn_id,
    prompt: interaction.prompt ?? interaction.title ?? '请选择下一步',
    options: (interaction.options ?? []).map((option) => ({
      id: option.value,
      label: option.label,
      impact: option.impact,
      recommended: option.recommended,
    })),
    status: interaction.status,
    expiresAt: interaction.expires_at,
    answer: interaction.answer,
  }
}

function noticeUi(notice: ChatNotice): NoticeUiData {
  return {
    key: notice.notice_id,
    severity: notice.severity,
    code: notice.code,
    message: notice.message,
  }
}

function messageItems(message: ChatMessage, keyPrefix: string): TurnItem[] {
  const streaming = message.status === 'streaming'
  const out: TurnItem[] = []
  const reasoning = message.parts.filter((part) => part.type === 'reasoning')
  if (reasoning.length > 0) {
    const text = reasoning.map((part) => part.text ?? '').join('')
    if (text.trim() !== '' || streaming) {
      out.push({
        type: 'reasoning',
        data: {
          key: `${keyPrefix}:reasoning:${message.message_id}`,
          text,
          streaming,
          done: !streaming,
        },
      })
    }
  }
  const textParts = message.parts.filter((part) => part.type === 'text')
  const text = textParts.map((part) => part.text ?? '').join('')
  if (text.trim() !== '' || streaming) {
    const report: ReportUiData = {
      key: `${keyPrefix}:text:${message.message_id}`,
      text,
      streaming,
      done: !streaming,
      purpose: message.purpose === 'final' ? 'final' : 'commentary',
    }
    out.push({ type: 'report', data: report })
  }
  if (message.status === 'failed' || message.status === 'interrupted') {
    out.push({
      type: 'notice',
      data: {
        key: `${keyPrefix}:notice:${message.message_id}`,
        severity: 'error',
        code: message.status === 'interrupted' ? 'MESSAGE_INTERRUPTED' : 'MESSAGE_FAILED',
        message: message.error || (message.status === 'interrupted' ? '该消息输出中断，已保留已保存内容' : '该消息输出失败'),
      },
    })
  }
  return out
}

function statusOf(turn: ChatTurn): TurnStatus {
  switch (turn.status) {
    case 'done':
      return 'completed'
    case 'canceled':
      return 'canceled'
    default:
      return turn.status
  }
}

const uiTurns = computed<TurnUiData[]>(() => {
  const byTurn = new Map<string, ChatItem[]>()
  for (const item of props.items) {
    const turnId =
      item.type === 'message'
        ? item.message.turn_id
        : item.type === 'tool'
          ? item.tool.turn_id
          : item.type === 'interaction'
            ? item.interaction.turn_id
            : item.notice.turn_id ?? ''
    const list = byTurn.get(turnId) ?? []
    list.push(item)
    byTurn.set(turnId, list)
  }
  return props.turns.map((turn, index) => {
    const items: TurnItem[] = []
    for (const item of byTurn.get(turn.turn_id) ?? []) {
      if (item.type === 'message') {
        items.push(...messageItems(item.message, turn.turn_id))
      } else if (item.type === 'tool') {
        items.push({ type: 'tool', data: toolUi(item.tool) })
      } else if (item.type === 'interaction') {
        if (item.interaction.kind === 'question') {
          items.push({ type: 'question', data: questionUi(item.interaction, turn.session_id) })
        } else {
          items.push({ type: 'approval', data: approvalUi(item.interaction) })
        }
      } else {
        items.push({ type: 'notice', data: noticeUi(item.notice) })
      }
    }
    return {
      key: turn.turn_id,
      sessionId: turn.session_id,
      seq: index + 1,
      input: turn.input ?? '',
      status: statusOf(turn),
      waitReason: turn.wait_reason,
      activity: turn.activity,
      error: turn.error,
      token: turn.usage ?? null,
      startedAt: turn.started_at,
      completedAt: turn.finished_at,
      items,
    }
  })
})

/** P7.2 智能跟随：默认跟随最新输出；用户上滑离开底部后暂停跟随。 */
const FOLLOW_THRESHOLD_PX = 24
const following = ref(true)
const unseenContent = ref(false)
const questions = computed(() => uiTurns.value.filter(turn => turn.input.trim()))
const highlightedQuestion = ref('')
const outlineOpen = ref(false)
let highlightTimer: ReturnType<typeof setTimeout> | undefined

function clearQuestionHighlight() {
  clearTimeout(highlightTimer)
  highlightedQuestion.value = ''
}

async function jumpToQuestion(key: string) {
  following.value = false
  unseenContent.value = false
  outlineOpen.value = false
  clearQuestionHighlight()
  highlightedQuestion.value = key
  await nextTick()
  if (highlightedQuestion.value !== key) return
  const target = Array.from(rollRef.value?.querySelectorAll('.turn-question') ?? [])
    .find(question => question.id === `question-${key}`)
  if (target) {
    const reducedMotion = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
    target.scrollIntoView?.({ behavior: reducedMotion ? 'instant' : 'smooth', block: 'center' })
  }
  highlightTimer = setTimeout(clearQuestionHighlight, 1600)
}

watch(() => props.turns[0]?.session_id, () => {
  clearQuestionHighlight()
  outlineOpen.value = false
  following.value = true
  unseenContent.value = false
})
onUnmounted(clearQuestionHighlight)

function distanceToBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight
}

function onScroll(): void {
  const el = rollRef.value
  if (!el) {
    return
  }
  following.value = distanceToBottom(el) <= FOLLOW_THRESHOLD_PX
  if (following.value) unseenContent.value = false
}

function scrollToBottom(): void {
  if (!following.value) {
    unseenContent.value = true
    return
  }
  void nextTick(() => {
    if (rollRef.value) {
      rollRef.value.scrollTop = rollRef.value.scrollHeight
    }
  })
}

function resumeFollow(): void {
  following.value = true
  unseenContent.value = false
  void nextTick(() => {
    rollRef.value?.scrollTo({ top: rollRef.value.scrollHeight, behavior: 'smooth' })
  })
}

watch(
  () => [props.turns, props.items],
  () => scrollToBottom(),
)

defineExpose({ uiTurns })
</script>

<template>
  <div class="timeline-layout">
    <aside v-if="questions.length" class="question-outline" :class="{ 'outline-open': outlineOpen }">
      <button
        class="outline-toggle"
        type="button"
        :aria-expanded="outlineOpen"
        @click="outlineOpen = !outlineOpen"
      >
        <List aria-hidden="true" /> 提问大纲
      </button>
      <nav class="outline-panel" aria-label="提问大纲">
        <div class="outline-title">提问大纲 <span>{{ questions.length }}</span></div>
        <ol>
          <li v-for="(question, index) in questions" :key="question.key">
            <button
              type="button"
              :title="question.input"
              :aria-current="highlightedQuestion === question.key ? 'location' : undefined"
              @click="jumpToQuestion(question.key)"
            >
              <span class="outline-index" aria-hidden="true">{{ index + 1 }}</span>
              <span class="outline-text">{{ question.input }}</span>
            </button>
          </li>
        </ol>
      </nav>
    </aside>
  <div ref="rollRef" class="timeline" @scroll.passive="onScroll">
    <template v-if="uiTurns.length">
      <div v-for="(turn, index) in uiTurns" :key="turn.key" class="turn-wrap rise-in">
        <TurnBlock
          :turn="turn"
          :latest="index === uiTurns.length - 1"
          :question-highlighted="highlightedQuestion === turn.key"
          :can-decide="canDecide"
          :cluster-name="clusterName"
          :can-branch="canBranch"
          @branch="emit('branch', $event)"
          @approve="(v: string, id?: string) => emit('approve', v, id)"
          @reject="(id?: string) => emit('reject', id)"
          @answer="(payload) => emit('answer', payload)"
        />
      </div>
    </template>
    <TechEmptyState v-else :title="props.emptyTitle" :hint="props.emptyHint" />
    <button v-if="!following && uiTurns.length" type="button" class="follow-latest" @click="resumeFollow">
      {{ unseenContent ? '有新内容，回到底部' : '回到底部' }}
    </button>
  </div>
  </div>
</template>

<style scoped>
.timeline-layout {
  position: relative;
  display: flex;
  flex: 1;
  min-height: 0;
  min-width: 0;
}

.question-outline {
  display: flex;
  flex: 0 0 172px;
  min-height: 0;
  padding: 16px 0 16px 12px;
}

.outline-panel {
  align-self: flex-start;
  max-height: 100%;
  width: 100%;
  overflow-y: auto;
  box-sizing: border-box;
  padding: 8px;
  border: 1px solid var(--app-border);
  border-radius: 8px;
  background: var(--app-surface);
  color: var(--app-muted);
  font-size: 12px;
}

.outline-title {
  display: flex;
  justify-content: space-between;
  padding: 4px 4px 8px;
  font-weight: 600;
}

.outline-panel ol {
  list-style: none;
  margin: 0;
  padding: 0;
}

.outline-panel button,
.outline-toggle {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 6px 4px;
  border: 0;
  border-radius: 4px;
  background: transparent;
  color: inherit;
  font: inherit;
  cursor: pointer;
  text-align: left;
}

.outline-panel button:hover,
.outline-panel button:focus-visible,
.outline-panel button[aria-current] {
  background: var(--app-accent-soft);
  color: var(--app-text);
}

.outline-index {
  flex: none;
  font-variant-numeric: tabular-nums;
}

.outline-text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.outline-toggle {
  display: none;
}

@media (max-width: 1050px) {
  .timeline-layout {
    flex-direction: column;
  }

  .question-outline {
    flex: none;
    padding: 4px 16px 0;
    z-index: 2;
  }

  .outline-toggle {
    display: inline-flex;
    width: auto;
    color: var(--app-muted);
    font-size: 12px;
  }

  .outline-toggle svg {
    width: 16px;
    height: 16px;
  }

  .outline-panel {
    display: none;
    position: absolute;
    top: 34px;
    left: 16px;
    width: min(300px, calc(100% - 32px));
    max-height: min(280px, 60%);
    box-shadow: var(--app-shadow);
  }

  .outline-open .outline-panel {
    display: block;
  }
}

.timeline {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
  min-width: 0;
  padding: 16px;
  overflow-y: auto;
  position: relative;
}

.follow-latest {
  position: sticky;
  bottom: 4px;
  align-self: center;
  margin-top: -8px;
  padding: 6px 14px;
  border: 1px solid var(--app-border);
  border-radius: 999px;
  background: var(--app-surface-2);
  color: var(--app-text);
  box-shadow: var(--app-shadow);
  font-size: 12.5px;
  cursor: pointer;
}

.follow-latest:hover {
  border-color: var(--app-accent);
  color: var(--app-accent);
}

.turn-wrap {
  max-width: 820px;
  width: 100%;
  align-self: center;
}
</style>
