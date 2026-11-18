<script setup lang="ts">
/**
 * P4.7 审计中心（方案 28）：管理员的只读追溯入口。
 *
 * 页面只做三件事：把筛选条件下推给后端、渲染字段白名单、在详情里展开同一次
 * 操作的关联时间线。审批不在这里做——确认动作仍在 Chat 中完成，本页只展示
 * 已发生的事实（谁提出、谁确认、执行结果、验证结果）。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ChatDotRound, Document, Refresh, RefreshLeft, Search, View } from '@element-plus/icons-vue'

import {
  AUDIT_EVENT_TYPES,
  auditDecisionLabel,
  auditEventLabel,
  auditEventTone,
  auditRiskLabel,
  auditStatusLabel,
  auditStatusTone,
  describeAuditError,
  getAuditEvent,
  listAuditEvents,
} from '@/api/audit'
import type { AuditDetailResult, AuditEventView } from '@/api/types'
import TechEmptyState from '@/components/TechEmptyState.vue'

/** 一页 50 条，与后端默认值一致；后端上限 100。 */
const PAGE_SIZE = 50

const router = useRouter()

const events = ref<AuditEventView[]>([])
const loading = ref(false)
const loadingMore = ref(false)
const cursor = ref('')
const hasMore = ref(false)

// 时间区间保留 el-date-picker 的本地墙钟字符串，提交时再转成 UTC 的 RFC3339。
const filters = reactive({
  range: [] as string[],
  eventType: '',
  status: '',
  userId: '',
  clusterId: '',
  sessionId: '',
})

const STATUS_OPTIONS = [
  { value: 'success', label: '成功' },
  { value: 'error', label: '失败' },
  { value: 'pending', label: '待处理' },
]

const activeFilterCount = computed(() => {
  let n = 0
  if (filters.range && filters.range.length === 2) n += 1
  for (const value of [filters.eventType, filters.status, filters.userId, filters.clusterId, filters.sessionId]) {
    if (value.trim()) n += 1
  }
  return n
})

function rangeParams(): { from?: string; to?: string } {
  const [from, to] = filters.range ?? []
  if (!from || !to) {
    return {}
  }
  return { from: new Date(from).toISOString(), to: new Date(to).toISOString() }
}

async function load(reset = true): Promise<void> {
  if (reset) {
    loading.value = true
    cursor.value = ''
  } else {
    loadingMore.value = true
  }
  try {
    const result = await listAuditEvents({
      ...rangeParams(),
      eventType: filters.eventType || undefined,
      status: filters.status || undefined,
      userId: filters.userId.trim() || undefined,
      clusterId: filters.clusterId.trim() || undefined,
      sessionId: filters.sessionId.trim() || undefined,
      limit: PAGE_SIZE,
      cursor: reset ? undefined : cursor.value || undefined,
    })
    events.value = reset ? result.items : [...events.value, ...result.items]
    cursor.value = result.nextCursor ?? ''
    hasMore.value = result.hasMore
  } catch (error) {
    ElMessage.error(describeAuditError('加载审计事件', error))
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

function resetFilters(): void {
  filters.range = []
  filters.eventType = ''
  filters.status = ''
  filters.userId = ''
  filters.clusterId = ''
  filters.sessionId = ''
  void load(true)
}

// ---------------------------------------------------------------- 详情

const drawerOpen = ref(false)
const detailLoading = ref(false)
const detail = ref<AuditDetailResult | null>(null)
const detailError = ref('')

async function openDetail(row: AuditEventView): Promise<void> {
  drawerOpen.value = true
  detail.value = null
  detailError.value = ''
  detailLoading.value = true
  try {
    detail.value = await getAuditEvent(row.id)
  } catch (error) {
    detailError.value = describeAuditError('读取审计详情', error)
    ElMessage.error(detailError.value)
  } finally {
    detailLoading.value = false
  }
}

function openSession(sessionId?: string): void {
  if (!sessionId) {
    return
  }
  void router.push({ name: 'chat', query: { session: sessionId } })
}

// ---------------------------------------------------------------- 展示口径

function formatTimestamp(value?: string): string {
  if (!value) {
    return '—'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  return date.toLocaleString('zh-CN', { hour12: false })
}

/** 操作者：后端解析不出名字时退回用户 ID；没有用户表示系统动作。 */
function operatorLabel(row: AuditEventView): string {
  if (!row.userId) {
    return '系统'
  }
  return row.userName || row.userId
}

/**
 * 操作对象：优先结构化资源名。没有资源名时只在 operation 与 action 不同才展示
 * operation——两者相同时还并排显示同一串文字，等于用两列说同一件事。
 */
function targetLabel(row: AuditEventView): string {
  if (row.resourceName) {
    return row.resourceKind ? row.resourceKind + '/' + row.resourceName : row.resourceName
  }
  if (row.operation && row.operation !== row.action) {
    return row.operation
  }
  return '—'
}

/** 列表里的摘要只取一段，完整内容在详情里看。 */
function summarySnippet(row: AuditEventView): string {
  return row.parameterSummary || row.resultSummary || row.matchReason || ''
}

function hasLink(row: AuditEventView): boolean {
  return Boolean(row.sessionId)
}

function prettySummary(raw?: string): string {
  if (!raw) {
    return ''
  }
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function detailMeta(row: AuditEventView): Array<{ label: string; value: string }> {
  const meta: Array<{ label: string; value: string }> = []
  const push = (label: string, value?: string): void => {
    if (value && value.trim()) {
      meta.push({ label, value })
    }
  }
  push('操作者', row.userId ? row.userName || row.userId : '系统')
  push('集群', row.clusterId)
  push('资源', targetLabel(row))
  push('动作', row.action)
  push('风险', auditRiskLabel(row.risk))
  push('决策', auditDecisionLabel(row.decision))
  push('结果', auditStatusLabel(row.status))
  push('关联审批', row.approvalId)
  push('关联授权', row.grantId)
  push('请求 ID', row.requestId)
  push('会话', row.sessionId)
  push('回合', row.turnId)
  return meta
}

onMounted(() => {
  void load(true)
})
</script>

<template>
  <section class="audit-page">
    <div class="page-head">
      <h2 class="title">
        审计中心
        <el-icon class="title-icon"><Document /></el-icon>
      </h2>
      <span class="desc">管理员只读追溯：谁提出、谁确认、实际执行了什么、结果如何。</span>
      <el-button text class="refresh" :icon="Refresh" :loading="loading" @click="load(true)">
        刷新
      </el-button>
    </div>

    <div class="card filters">
      <el-date-picker
        v-model="filters.range"
        type="datetimerange"
        range-separator="至"
        start-placeholder="开始时间"
        end-placeholder="结束时间"
        value-format="YYYY-MM-DDTHH:mm:ss"
        class="range"
      />
      <el-select v-model="filters.eventType" clearable placeholder="事件类型" class="w150">
        <el-option
          v-for="item in AUDIT_EVENT_TYPES"
          :key="item.value"
          :label="item.label"
          :value="item.value"
        />
      </el-select>
      <el-select v-model="filters.status" clearable placeholder="结果" class="w120">
        <el-option v-for="item in STATUS_OPTIONS" :key="item.value" :label="item.label" :value="item.value" />
      </el-select>
      <el-input v-model="filters.userId" clearable placeholder="操作者 ID" class="w150" />
      <el-input v-model="filters.clusterId" clearable placeholder="集群 ID" class="w150" />
      <el-input v-model="filters.sessionId" clearable placeholder="会话 ID" class="w170" />
      <el-button type="primary" :icon="Search" @click="load(true)">查询</el-button>
      <el-button text :icon="RefreshLeft" @click="resetFilters">重置</el-button>
    </div>

    <div class="card list-card">
      <div class="row list-head">
        <span class="sub strong">事件流水</span>
        <span class="pill mute">{{ events.length }} 条</span>
        <span v-if="activeFilterCount > 0" class="pill info">{{ activeFilterCount }} 个筛选</span>
        <span class="faint grow">按时间倒序；打开一条才展开同一次操作的完整链路</span>
      </div>

      <div v-if="loading" class="center-hint muted">加载中…</div>
      <TechEmptyState
        v-else-if="events.length === 0"
        title="没有匹配的审计事件"
        hint="调整时间范围或筛选项后重试。审计读取与聊天运行互相独立，页面故障不会影响 Chat。"
      />
      <div v-else class="event-list">
        <button
          v-for="row in events"
          :key="row.id"
          type="button"
          class="event-row"
          @click="openDetail(row)"
        >
          <span class="mono time">{{ formatTimestamp(row.timestamp) }}</span>
          <span class="operator" :title="row.userId || '系统'">{{ operatorLabel(row) }}</span>
          <span class="pill" :class="auditEventTone(row.eventType)">{{ auditEventLabel(row.eventType) }}</span>
          <span class="target" :title="targetLabel(row)">{{ targetLabel(row) }}</span>
          <span class="mono action" :title="row.action">{{ row.action }}</span>
          <span class="mono cluster">{{ row.clusterId || '—' }}</span>
          <span class="pill" :class="auditStatusTone(row.status)">{{ auditStatusLabel(row.status) }}</span>
          <span class="snippet" :title="summarySnippet(row)">{{ summarySnippet(row) }}</span>
          <el-icon class="go"><View /></el-icon>
        </button>
      </div>

      <div v-if="hasMore" class="more">
        <el-button text :loading="loadingMore" @click="load(false)">加载更多</el-button>
      </div>
    </div>

    <el-drawer v-model="drawerOpen" title="审计详情" size="600px" class="audit-drawer">
      <div v-if="detailLoading" class="center-hint muted">加载中…</div>
      <div v-else-if="detailError" class="center-hint muted">{{ detailError }}</div>
      <template v-else-if="detail">
        <div class="detail-head">
          <div class="detail-title">
            <span class="pill" :class="auditEventTone(detail.event.eventType)">
              {{ auditEventLabel(detail.event.eventType) }}
            </span>
            <span class="mono">{{ detail.event.action }}</span>
          </div>
          <div class="muted small">{{ formatTimestamp(detail.event.timestamp) }}</div>
        </div>

        <dl class="meta-grid">
          <template v-for="item in detailMeta(detail.event)" :key="item.label">
            <dt>{{ item.label }}</dt>
            <dd :class="{ mono: item.label.includes('ID') || item.label === '会话' || item.label === '回合' }">
              {{ item.value }}
            </dd>
          </template>
        </dl>

        <div v-if="detail.event.parameterSummary" class="block">
          <div class="block-title">参数摘要</div>
          <pre class="summary">{{ prettySummary(detail.event.parameterSummary) }}</pre>
        </div>
        <div v-if="detail.event.resultSummary" class="block">
          <div class="block-title">结果摘要</div>
          <pre class="summary">{{ prettySummary(detail.event.resultSummary) }}</pre>
        </div>
        <div v-if="!detail.event.parameterSummary && !detail.event.resultSummary" class="muted small">
          该事件没有摘要字段（历史记录未写入）。结论仍以事件类型与结果为准。
        </div>

        <div class="block">
          <div class="block-title">
            关联时间线
            <span class="pill mute">{{ detail.timeline.length }} 条</span>
            <span v-if="detail.timelineTruncated" class="pill warning">已截断</span>
          </div>
          <div v-if="detail.timeline.length === 0" class="muted small">
            没有可关联的事件：这条记录本身不带审批、请求或回合关联键。
          </div>
          <ol v-else class="timeline">
            <li
              v-for="row in detail.timeline"
              :key="row.id"
              class="timeline-item"
              :class="{ current: row.id === detail.event.id }"
            >
              <button type="button" class="timeline-btn" @click="openDetail(row)">
                <span class="mono time">{{ formatTimestamp(row.timestamp) }}</span>
                <span class="pill" :class="auditEventTone(row.eventType)">{{ auditEventLabel(row.eventType) }}</span>
                <span class="mono action">{{ row.action }}</span>
                <span class="pill" :class="auditStatusTone(row.status)">{{ auditStatusLabel(row.status) }}</span>
              </button>
            </li>
          </ol>
        </div>

        <div v-if="hasLink(detail.event)" class="block">
          <el-button :icon="ChatDotRound" @click="openSession(detail.event.sessionId)">
            打开原会话（沿用 Chat 的访问校验）
          </el-button>
        </div>
      </template>
    </el-drawer>
  </section>
</template>

<style scoped>
.audit-page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  min-height: 0;
}

.page-head {
  display: flex;
  align-items: center;
  gap: 12px;
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
  padding: 14px 16px;
}

.page-head .title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-size: 16px;
}

.title-icon {
  color: var(--app-accent-2);
}

.page-head .desc {
  font-size: 12.5px;
  color: var(--app-muted);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.refresh {
  margin-left: auto;
}

.card {
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
}

.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
}

.filters .range {
  max-width: 380px;
}

.w120 {
  width: 120px;
}

.w150 {
  width: 150px;
}

.w170 {
  width: 170px;
}

.list-card {
  display: flex;
  flex-direction: column;
  padding: 14px 16px;
  min-height: 240px;
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.list-head {
  margin-bottom: 10px;
}

.sub {
  font-size: 13px;
}

.strong {
  font-weight: 650;
}

.grow {
  min-width: 0;
  flex: 1;
}

.faint {
  color: var(--app-faint, var(--app-muted));
  opacity: 0.8;
}

.muted {
  color: var(--app-muted);
}

.small {
  font-size: 12px;
}

.mono {
  font-family: var(--app-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
}

.pill {
  display: inline-flex;
  align-items: center;
  flex: none;
  padding: 2px 8px;
  font-size: 11px;
  line-height: 1.4;
  border-radius: 999px;
  background: rgba(99, 102, 241, 0.12);
  color: var(--app-accent);
}

.pill.info {
  background: rgba(99, 102, 241, 0.12);
  color: var(--app-accent);
}

.pill.success {
  background: rgba(52, 211, 153, 0.12);
  color: #34d399;
}

.pill.warning {
  background: rgba(251, 191, 36, 0.12);
  color: #fbbf24;
}

.pill.danger {
  background: rgba(248, 113, 113, 0.12);
  color: #f87171;
}

.pill.mute {
  background: var(--app-surface-hover);
  color: var(--app-muted);
}

.center-hint {
  padding: 24px 8px;
  text-align: center;
  font-size: 12.5px;
}

.event-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

/* 每行是一条可点击的事件：信息密度高，但不做表格（窄屏下表格会横向溢出） */
.event-row {
  display: grid;
  grid-template-columns: 150px minmax(90px, 130px) auto minmax(120px, 1fr) minmax(110px, 180px) 80px 64px minmax(0, 1.2fr) 18px;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 9px 10px;
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
  color: var(--app-text);
  font-size: 12px;
  text-align: left;
  cursor: pointer;
  transition: border-color 0.15s ease, background 0.15s ease;
}

.event-row:hover {
  border-color: var(--app-accent);
  background: var(--app-surface-hover);
}

.event-row .time {
  color: var(--app-muted);
  white-space: nowrap;
}

.event-row .operator {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.event-row .target,
.event-row .snippet {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.event-row .snippet {
  color: var(--app-muted);
}

.event-row .go {
  color: var(--app-muted);
}

.more {
  display: flex;
  justify-content: center;
  padding-top: 8px;
}

/* ---------- 详情形样式（drawer teleport 到 body，放全局块） ---------- */
</style>

<style>
.audit-drawer .detail-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  padding-bottom: 10px;
  border-bottom: 1px solid var(--app-hairline);
}

.audit-drawer .detail-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}

.audit-drawer .meta-grid {
  display: grid;
  grid-template-columns: 84px minmax(0, 1fr);
  gap: 6px 10px;
  margin: 14px 0;
  font-size: 12.5px;
}

.audit-drawer .meta-grid dt {
  color: var(--app-muted);
}

.audit-drawer .meta-grid dd {
  margin: 0;
  word-break: break-all;
}

.audit-drawer .block {
  margin-top: 18px;
}

.audit-drawer .block-title {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-size: 13px;
  font-weight: 650;
}

.audit-drawer .summary {
  margin: 0;
  padding: 10px 12px;
  max-height: 220px;
  overflow: auto;
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
  color: var(--app-text);
  font-family: var(--app-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
  font-size: 11.5px;
  white-space: pre-wrap;
  word-break: break-word;
}

.audit-drawer .timeline {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.audit-drawer .timeline-item {
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
}

.audit-drawer .timeline-item.current {
  border-color: var(--app-accent);
  box-shadow: inset 2px 0 0 var(--app-accent);
}

.audit-drawer .timeline-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 8px 10px;
  border: 0;
  background: transparent;
  color: var(--app-text);
  font-size: 12px;
  text-align: left;
  cursor: pointer;
}

.audit-drawer .timeline-btn .time {
  color: var(--app-muted);
  white-space: nowrap;
}

.audit-drawer .timeline-btn .action {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
