<script setup lang="ts">
import { computed, ref } from 'vue'
import { ArrowDown, WarningFilled } from '@element-plus/icons-vue'

import type { TurnItem, TurnUiData } from './turn'
import { formatDuration } from './turn'
import ProcessGroup from './ProcessGroup.vue'
import ReasoningPanel from './ReasoningPanel.vue'
import ToolCallCard from './ToolCallCard.vue'
import ApprovalCard from './ApprovalCard.vue'
import QuestionInteraction from './QuestionInteraction.vue'
import MarkdownView from './MarkdownView.vue'
import copyText from '@/composables/useClipboard'

/**
 * 单回合容器：用户输入 + 真实过程条目（消息/工具/问题/审批/告警）。
 * 不再渲染固定“思考→工具→观察→报告”步进器，运行状态只来自 Turn 状态与 activity。
 */

const props = withDefaults(
  defineProps<{
    turn: TurnUiData
    /** P2.9 当前用户是否有权审批（本人自确认）。 */
    canDecide?: boolean
    /** P12.4 当前会话绑定的集群名称。 */
    clusterName?: string
  }>(),
  {
    canDecide: true,
    clusterName: '',
  },
)

const emit = defineEmits<{
  (e: 'approve', value: string,id?:string): void
  (e: 'reject',id?:string): void
  (e: 'answer', payload: { interactionId: string; status: 'selected' | 'text' | 'skipped'; optionId?: string; text?: string }): void
}>()

const open = ref(true)
// P7.4：展开/折叠全部工具卡（数值信号驱动子卡片），报告完成后未手动操作过的卡片自动折叠。
const collapseAllSignal = ref(0)
const expandAllSignal = ref(0)

const statusType = computed<'primary' | 'success' | 'danger' | 'warning' | 'info'>(() => {
  switch (props.turn.status) {
    case 'running':
      return 'primary'
    case 'completed':
      return 'success'
    case 'failed':
      return 'danger'
    case 'waiting':
      return 'warning'
    case 'canceled':
      return 'info'
    default:
      return 'info'
  }
})

const statusLabel = computed(() => {
  switch (props.turn.status) {
    case 'running':
      return activityText.value || '运行中'
    case 'completed':
      return '已完成'
    case 'failed':
      return '失败'
    case 'canceled':
      return '已取消'
    case 'waiting':
      return waitText.value
    case 'pending':
      return '已接收'
    default:
      return props.turn.status
  }
})

/** 等待原因来自 turn.wait_reason，不与连接状态混用。 */
const waitText = computed(() => {
  switch (props.turn.waitReason) {
    case 'question':
      return '等待你选择'
    case 'approval':
      return '等待你审批'
    case 'secret':
      return '等待敏感输入'
    default:
      return '等待你的确认'
  }
})

/** 运行中措辞只由真实 activity 产生。 */
const activityText = computed(() => {
  switch (props.turn.activity) {
    case 'checking':
      return '检查中'
    case 'generating':
      return '生成回复'
    case 'executing':
      return '执行中'
    case 'verifying':
      return '验证中'
    default:
      return ''
  }
})

const duration = computed(() => formatDuration(turnDurationMs(props.turn)))

function turnDurationMs(turn: TurnUiData): number | null {
  const start = turn.startedAt ? new Date(turn.startedAt).getTime() : NaN
  const end = turn.completedAt ? new Date(turn.completedAt).getTime() : NaN
  if (Number.isNaN(start)) return null
  return (Number.isNaN(end) ? Date.now() : end) - start
}

const tasks = computed(() => props.turn.items.filter(item => item.type === 'tool'))
const active = computed(() => ['pending', 'running', 'waiting'].includes(props.turn.status))
const tasksOpen = ref<boolean | null>(null)
const unknownTask = computed(() => tasks.value.some(task => task.data.status === 'unknown'))

function taskStatus(task: (typeof tasks.value)[number]['data']): string {
  if (active.value && task.status === 'running') {
    const approval = props.turn.items.find(item =>
      item.type === 'approval' && item.data.toolCallId === task.key && item.data.decision === 'pending',
    )
    if (approval?.type === 'approval') {
      if (approval.data.operation === 'secret.read') return '等待同意读取 Secret'
      return approval.data.secret ? '等待提供所需的值' : '等待人工确认'
    }
  }
  if (task.denied) return '未获批准，未执行'
  switch (task.status) {
    case 'done': return '已完成'
    case 'error': return '失败或受限'
    case 'unknown': return '结果未知'
    default: return !active.value ? '未取得结果' : '处理中'
  }
}

const reportDone = computed(() =>
  props.turn.items.some((item) => item.type === 'report' && item.data.purpose === 'final' && item.data.done),
)

const groups = computed(() => {
  const result: Array<{ key: string; process: boolean; items: TurnItem[] }> = []
  for (const item of props.turn.items) {
    const process = item.type === 'tool' || item.type === 'reasoning' ||
      (item.type === 'report' && item.data.purpose !== 'final')
    const previous = result[result.length - 1]
    if (process && previous?.process) previous.items.push(item)
    else result.push({ key: item.data.key, process, items: [item] })
  }
  return result
})



function timeLabel(value?: string): string {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/**
 * P7.2 Run Bar：给回合内条目生成稳定锚点 id，并把步进器步骤映射到对应条目，
 * 点击步骤即可滚动定位（配合 CSS 的 scroll-margin-top 避让 sticky 顶栏）。
 */
function itemAnchor(item: { data: { key: string } }): string {
  return `step-${props.turn.key}-${item.data.key}`
}
</script>

<template>
  <section class="turn" :class="turn.status">
    <header class="turn-head">
      <button type="button" class="turn-toggle" @click="open = !open" :aria-expanded="open">
        <el-icon class="turn-caret" :class="{ open }"><ArrowDown /></el-icon>
        <span class="turn-seq">{{ turn.seq }}</span>
        <span class="turn-input">{{ turn.input || '（历史会话）' }}</span>
      </button>
      <div class="turn-meta">
        <span v-if="turn.startedAt" class="turn-time">{{ timeLabel(turn.startedAt) }}</span>
        <span v-if="duration" class="turn-duration">{{ duration }}</span>
        <el-tag size="small" effect="plain" :type="statusType" class="turn-status">{{
          statusLabel
        }}</el-tag>
      </div>
    </header>

    <div v-if="open" class="turn-body">
      <!-- 用户输入：与原型一致，回合内先回显用户诉求，再展示执行过程 -->
      <article v-if="turn.input" class="turn-question">
        <span class="question-role">你</span>
        <div>
          <p class="question-text">{{ turn.input }}</p>
          <el-button text size="small" @click="copyText(turn.input)">复制</el-button>
        </div>
      </article>

      <div v-if="turn.error" class="turn-error">
        <el-icon><WarningFilled /></el-icon>
        <span>{{ turn.error }}</span>
      </div>

      <div v-if="tasks.length" class="turn-tasks">
        <button type="button" :aria-expanded="tasksOpen ?? active" @click="tasksOpen = !(tasksOpen ?? active)">
          本轮任务 · {{ tasks.length }} 项
        </button>
        <ul v-if="tasksOpen ?? active">
          <li v-for="task in tasks" :key="task.data.key">
            <strong>{{ task.data.shortName }}</strong> · {{ taskStatus(task.data) }}
            <span v-if="task.data.summary"> — {{ task.data.summary }}</span>
          </li>
        </ul>
      </div>
      <p v-if="unknownTask" class="turn-error" role="alert">有操作结果未知，请先核实集群现场，不要直接重试。</p>
      <div class="turn-items">
        <component
          :is="group.process ? ProcessGroup : 'div'"
          v-for="group in groups"
          :key="group.key"
          :items="group.process ? group.items : undefined"
          :completed="turn.status === 'completed' && reportDone"
          :failed="turn.status === 'failed' || turn.status === 'canceled'"
        >
        <template
          v-for="item in group.items"
          :key="item.type === 'reasoning' || item.type === 'report' ? item.data.key : item.data.key"
        >
          <ReasoningPanel
            v-if="item.type === 'reasoning'"
            :id="itemAnchor(item)"
            class="turn-item"
            :text="item.data.text"
            :streaming="item.data.streaming"
          />
          <ToolCallCard
            v-else-if="item.type === 'tool'"
            :id="itemAnchor(item)"
            class="turn-item"
            :tool="item.data"
            :session-id="turn.sessionId"
            :auto-open="false"
            :report-done="reportDone"
            :collapse-all="collapseAllSignal"
            :expand-all="expandAllSignal"
          />
          <ApprovalCard
            v-else-if="item.type === 'approval'"
            class="turn-item"
            :approval="item.data"
            :can-decide="canDecide"
            :cluster-label="clusterName"
            @approve="(v,id)=>emit('approve',v,id)"
            @reject="emit('reject',$event)"
          />
          <QuestionInteraction
            v-else-if="item.type === 'question'"
            class="turn-item"
            :question="item.data"
            @answer="(payload)=>emit('answer',payload)"
          />
          <p
            v-else-if="item.type === 'notice'"
            class="turn-item notice"
            :class="item.data.severity"
          >
            {{ item.data.message }}
          </p>
          <article
            v-else-if="item.type === 'report'"
            :id="itemAnchor(item)"
            class="turn-item report-block"
            :class="{ streaming: item.data.streaming, commentary: item.data.purpose !== 'final' }"
          >
            <div class="report-head">
              <span class="report-label">{{ item.data.purpose === 'final' ? '诊断回复' : '分析' }}</span>
              <span v-if="item.data.streaming" class="report-hint">正在生成…</span>
            </div>
            <MarkdownView :text="item.data.text" />
            <el-button v-if="item.data.purpose === 'final' && item.data.done" text size="small" @click="copyText(item.data.text)">复制</el-button>
          </article>
        </template>
        </component>
      </div>
    </div>
  </section>
</template>

<style scoped>
.turn-tasks {
  padding: 8px 12px;
  border: 1px solid var(--app-border);
  border-radius: 8px;
  font-size: 13px;
}
.turn-tasks button {
  border: 0;
  background: transparent;
  color: inherit;
  font: inherit;
  cursor: pointer;
}
.turn-tasks ul {
  margin: 8px 0 0;
  padding-inline-start: 24px;
  overflow-wrap: anywhere;
}
.report-block.commentary {
  border: 0;
  background: transparent;
  padding: 4px 0;
  color: var(--app-muted);
}
.report-block.commentary .report-head { display: none; }

.turn {
  /* 对齐原型：回合本身是透明的时间线分组（卡片感由内部工具卡/报告块提供） */
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.turn-head {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 2px;
}

.turn-toggle {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  border: none;
  background: transparent;
  padding: 0;
  color: var(--app-text);
  font-family: inherit;
  cursor: pointer;
  text-align: left;
  flex: 1;
}

.turn-caret {
  flex: none;
  color: var(--app-muted);
  transition: transform 0.2s ease;
}

.turn-caret.open {
  transform: rotate(180deg);
}

.turn-seq {
  flex: none;
  color: var(--app-faint);
  font-family: var(--app-mono);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.5px;
}

.turn-seq::before {
  content: 'T';
}

.turn-input {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: 650;
  font-size: 13px;
}

.turn-meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: none;
}

.turn-time {
  font-size: 11px;
  color: var(--app-muted);
  font-variant-numeric: tabular-nums;
}

.turn-duration {
  font-size: 11px;
  color: var(--app-muted);
  font-variant-numeric: tabular-nums;
}

.turn-token {
  font-size: 11px;
  color: var(--app-accent);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.turn-status {
  text-transform: capitalize;
}

.turn-body {
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
}

.turn-question {
  /* 对齐原型 .msg.user：accent 描边 + accent-soft 底色，右对齐 */
  align-self: flex-end;
  max-width: 820px;
  padding: 11px 14px;
  border: 1px solid var(--app-accent);
  border-radius: var(--app-radius);
  background: var(--app-accent-soft);
}

.question-role {
  color: var(--app-faint);
  font-size: 11px;
}

.question-text {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.turn-error {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 7px 10px;
  border-radius: 6px;
  background: var(--app-danger-soft);
  color: var(--app-danger);
  font-size: 12px;
}

.turn-tools {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 2px 0;
}

.turn-tools-btn {
  padding: 2px 10px;
  border: 1px solid var(--app-border);
  border-radius: 999px;
  background: transparent;
  color: var(--app-muted);
  font-size: 11.5px;
  cursor: pointer;
}

.turn-tools-btn:hover {
  border-color: var(--app-accent);
  color: var(--app-accent);
}

.turn-items {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.report-block {
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
  padding: 16px 20px;
  background: var(--app-surface);
  font-size: 13.5px;
  line-height: 1.65;
}

.report-block.streaming {
  border-color: var(--app-accent);
}

.report-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}

.report-label {
  font-weight: 650;
  font-size: 11.5px;
  color: var(--app-muted);
  letter-spacing: 0.3px;
  text-transform: uppercase;
}

.report-hint {
  font-size: 11px;
  color: var(--app-accent);
  animation: report-pulse 1.1s ease-in-out infinite;
}

@keyframes report-pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.4;
  }
}

/* P7.2：Run Bar 定位时避开 sticky 顶栏 */
.turn-item {
  scroll-margin-top: 56px;
}

.notice {
  margin: 0;
  padding: 8px 12px;
  border: 1px solid var(--app-border);
  border-radius: 8px;
  background: var(--app-surface-2);
  color: var(--app-muted);
  font-size: 12px;
  line-height: 1.6;
}

.notice.warning {
  border-color: var(--app-warning-border, var(--app-border));
  color: var(--app-warning, var(--app-text));
}

.notice.error {
  border-color: var(--app-danger-border);
  background: var(--app-danger-soft);
  color: var(--app-danger);
}
</style>
