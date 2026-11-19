<script setup lang="ts">
import { computed, ref, useId } from 'vue'

import type { ChatInteraction, ChatToolCall, ChatTurn } from '@/api/chat'

const props = defineProps<{
  tools: ChatToolCall[]
  turn: ChatTurn | undefined
  pending: ChatInteraction | null
  stopping: boolean
}>()

const panelId = useId()
const hovered = ref(false)
const pinned = ref(false)
const open = computed(() => hovered.value || pinned.value)
const active = computed(
  () =>
    props.turn?.status === 'running' ||
    props.turn?.status === 'waiting' ||
    props.turn?.status === 'pending',
)
const toolStatusLabels = {
  started: '待执行',
  running: '执行中',
  succeeded: '已完成',
  failed: '执行失败',
  denied: '未获批准，未执行',
  unknown: '结果未知',
} satisfies Record<ChatToolCall['status'], string>

function toolLabel(tool: ChatToolCall): string {
  return tool.command?.match(/\bkubectl\s+([\w-]+)/i)?.[1] ?? tool.name
}

const rows = computed(() =>
  props.tools.map((tool) => {
    const unfinished = tool.status === 'running' || tool.status === 'started'
    let tone: ChatToolCall['status'] | 'waiting' = tool.status
    let status = toolStatusLabels[tool.status]
    if (unfinished) {
      if (!active.value) {
        tone = 'unknown'
        status = '未取得结果'
      } else if (props.stopping) {
        tone = 'waiting'
        status = '正在停止'
      } else if (props.pending?.tool_call_id === tool.tool_call_id) {
        tone = 'waiting'
        status =
          props.pending.kind === 'question'
            ? '等待补充信息'
            : props.pending.operation === 'secret.read'
              ? '等待读取确认'
              : '等待人工确认'
      }
    }
    return { tool, name: toolLabel(tool), tone, status }
  }),
)

const indicator = computed(() => {
  const count = rows.value.length
  const running = rows.value.find((row) => row.tone === 'running')
  if (props.stopping) return { tone: 'waiting', text: '正在停止' }
  if (rows.value.some((row) => row.tool.status === 'unknown'))
    return { tone: 'unknown', text: '有任务结果未知' }
  if (props.turn?.status === 'waiting') {
    const text =
      props.turn.wait_reason === 'question'
        ? '等待补充信息'
        : props.turn.wait_reason === 'secret'
          ? '等待提供所需的值'
          : '等待人工确认'
    return { tone: 'waiting', text }
  }
  if (running) return { tone: 'running', text: `执行中 · ${running.name}` }
  if (props.turn?.status === 'canceled')
    return { tone: 'waiting', text: `已停止 · ${count} 项任务` }
  if (
    rows.value.some(
      (row) => row.tone === 'failed' || row.tone === 'denied' || row.tone === 'unknown',
    )
  ) {
    return { tone: 'waiting', text: `${count} 项任务 · 有未完成项` }
  }
  if (rows.value.every((row) => row.tone === 'succeeded'))
    return { tone: 'succeeded', text: `已完成 · ${count} 项任务` }
  return { tone: 'started', text: `已发现 ${count} 项任务` }
})

function close(): void {
  hovered.value = false
  pinned.value = false
}

function handleFocusOut(event: FocusEvent): void {
  if (
    event.currentTarget instanceof HTMLElement &&
    !event.currentTarget.contains(event.relatedTarget as Node | null)
  ) {
    pinned.value = false
  }
}
</script>

<template>
  <div
    v-if="rows.length"
    class="task-status"
    @mouseenter="hovered = true"
    @mouseleave="hovered = false"
    @focusout="handleFocusOut"
    @keydown.esc.stop="close"
  >
    <button
      type="button"
      class="task-trigger"
      :aria-expanded="open"
      :aria-controls="open ? panelId : undefined"
      @click="pinned ? close() : (pinned = true)"
    >
      <span class="task-dot" :class="indicator.tone" aria-hidden="true" />
      {{ indicator.text }}
      <span aria-hidden="true">⌃</span>
    </button>
    <div v-if="open" :id="panelId" class="task-popover">
      <div class="task-panel">
        <div class="task-heading">本轮任务 · {{ rows.length }} 项</div>
        <ul class="task-list" tabindex="0" aria-label="本轮任务">
          <li
            v-for="row in rows"
            :key="row.tool.tool_call_id"
            v-memo="[row.tool, row.tone, row.status]"
            class="task-row"
          >
            <span class="task-dot" :class="row.tone" aria-hidden="true" />
            <div>
              <div class="task-name">{{ row.name }}</div>
              <div v-if="row.tool.summary || row.tool.error" class="task-summary">
                {{ row.tool.summary || row.tool.error }}
              </div>
            </div>
            <span class="task-label">{{ row.status }}</span>
          </li>
        </ul>
      </div>
    </div>
  </div>
</template>

<style scoped>
.task-status {
  position: relative;
}
.task-trigger {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 5px 9px;
  border: 1px solid transparent;
  border-radius: 6px;
  background: transparent;
  color: var(--app-text);
  font-size: 12px;
  cursor: pointer;
}
.task-trigger:hover,
.task-trigger[aria-expanded='true'] {
  border-color: var(--app-border);
  background: var(--app-surface);
}
.task-popover {
  position: absolute;
  z-index: 5;
  inset-block-end: 100%;
  inset-inline-start: 50%;
  transform: translateX(-50%);
  inline-size: min(340px, 75vw);
  padding-block-end: 7px;
}
.task-panel {
  border: 1px solid var(--app-border);
  border-radius: 10px;
  background: var(--app-surface);
  box-shadow: var(--app-shadow);
}
.task-heading {
  padding: 9px 12px;
  border-bottom: 1px solid var(--app-border);
  font-size: 12px;
  color: var(--app-muted);
}
.task-list {
  list-style: none;
  margin: 0;
  padding: 4px 12px;
  max-height: min(260px, 36vh);
  overflow: auto;
}
.task-row {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  gap: 8px;
  align-items: baseline;
  padding: 7px 0;
  font-size: 12px;
}
.task-row + .task-row {
  border-top: 1px solid var(--app-border);
}
.task-name,
.task-summary {
  overflow-wrap: anywhere;
}
.task-name {
  font-family: var(--app-mono);
}
.task-label,
.task-summary {
  color: var(--app-muted);
}
.task-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--app-muted);
  flex: none;
}
.task-dot.succeeded {
  background: var(--app-success);
}
.task-dot.running {
  background: var(--app-accent);
  animation: task-pulse 1.2s 300ms ease-in-out infinite;
}
.task-dot.waiting,
.task-dot.unknown {
  background: var(--app-warning);
}
.task-dot.failed,
.task-dot.denied {
  background: var(--app-danger);
}
@keyframes task-pulse {
  50% {
    opacity: 0.35;
  }
}
@media (prefers-reduced-motion: reduce) {
  .task-dot.running {
    animation: none;
  }
}
@media (max-width: 640px) {
  .task-status {
    position: static;
  }
}
</style>
