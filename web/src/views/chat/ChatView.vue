<script setup lang="ts">
import DiagnosticMemories from './DiagnosticMemories.vue'
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ChatDotRound, RefreshRight } from '@element-plus/icons-vue'

import type { ChatToolCall } from '@/api/chat'
import type { SessionSummary } from '@/api/sessions'
import { branchSession, getBranchOrigin, listChatModels, type BranchOrigin } from '@/api/sessions'
import {
  answerInteraction,
  approveRun,
  cancelRun,
  createSession,
  deleteSession,
  listSessions,
  pinSession,
  renameSession,
  sendMessage,
} from '@/api/sessions'
import ConversationList from '@/components/chat/ConversationList.vue'
import MessageTimeline from '@/components/chat/MessageTimeline.vue'
import ComposerBox from '@/components/chat/ComposerBox.vue'
import TaskStatusBar from '@/components/chat/TaskStatusBar.vue'
import TokenUsageBadge from '@/components/chat/TokenUsageBadge.vue'
import TechEmptyState from '@/components/TechEmptyState.vue'
import {useClusterAccess} from '@/composables/useClusterAccess'
import { buildSessionMarkdown, markdownFileName } from '@/utils/exportMarkdown'
import { useChatStream, type StreamStatus } from '@/composables/useChatStream'
import { useAuthStore } from '@/stores/auth'
import { sessionStateTone } from '@/utils/format'
import { canDecideApproval, hasPermission, PERM, permissionsOf } from '@/utils/permission'
import type { Skill } from '@/api/types'

const ACTIVE_SESSION_KEY = 'aegisops_active_session'

const sessions = ref<SessionSummary[]>([])
const loading = ref(false)
const activeId = ref<string | null>(sessionStorage.getItem(ACTIVE_SESSION_KEY))
const stream = useChatStream()

const input = ref('')
const sending = ref(false)
const modelOptions = ref<string[]>([])
const modelLabels = ref<Record<string, string>>({})
const selectedModel = ref('')
const nextModel = computed(() => selectedModel.value || modelOptions.value[0] || '')
async function refreshModels() {
  try {
    const models = await listChatModels()
    modelOptions.value = models.map(model => model.id)
    modelLabels.value = Object.fromEntries(models.map(model => [model.id, model.name || model.id]))
    if (!modelOptions.value.includes(selectedModel.value)) selectedModel.value = ''
  } catch {
    modelOptions.value = []
  }
}
const latestTurn = computed(() => {
  const turns = stream.turns.value
  const turn = turns[turns.length - 1]
  return turn?.session_id === activeId.value ? turn : undefined
})
const latestTools = computed<ChatToolCall[]>((previous) => {
  const turnId = latestTurn.value?.turn_id
  const tools = stream.items.value.flatMap(item =>
    item.type === 'tool' && item.tool.turn_id === turnId ? [item.tool] : [],
  )
  return previous?.length === tools.length && tools.every((tool, index) => tool === previous[index])
    ? previous
    : tools
})
const runStatus = computed(() => {
  const turn = latestTurn.value
  if (turn?.status === 'waiting') {
    if (turn.wait_reason === 'question') return '等待你补充信息'
    if (turn.wait_reason === 'secret') return '等待你提供所需的值'
    return '等待你确认'
  }
  if (turn?.status === 'running') return '处理中'
  if (turn?.status === 'pending') return '已接收'
  if (turn?.status === 'failed') return '上一轮未完成'
  if (turn?.status === 'canceled') return '上一轮已停止'
  return ''
})
const contextBudget = computed(() => {
  const sample = latestTurn.value?.context_budget
  if (!stream.running.value && nextModel.value && sample?.model_id && sample.model_id !== nextModel.value) return undefined
  return sample
})
const contextPercent = computed(() => {
  const sample = contextBudget.value
  if (!sample || sample.unavailable || sample.window <= sample.output_reserve) return null
  return (100 * sample.input_estimate) / (sample.window - sample.output_reserve)
})
const contextDetails = computed(() => {
  const sample = contextBudget.value
  const percent = contextPercent.value
  if (!sample || percent === null) return '暂无可展示的请求预算'
  const inputBudget = sample.window - sample.output_reserve
  return `总窗口 ${sample.window.toLocaleString()} · 输出预留 ${sample.output_reserve.toLocaleString()} · 可用输入 ${inputBudget.toLocaleString()} · 估算已用 ${sample.input_estimate.toLocaleString()} · 约剩余 ${Math.max(0, Math.round(100 - percent))}%`
})
const stoppingSession = ref('')
const branchOrigin = ref<BranchOrigin | null>(null)
const branching = ref(false)
let pendingBranch: { sessionId: string; turnId: string; key: string } | undefined
const composerRef = ref<InstanceType<typeof ComposerBox> | null>(null)
const auth = useAuthStore()
const {clusters,refresh:loadScope}=useClusterAccess()
const showClusterPicker = ref(false)
const clusterQuery = ref('')
const creatingSession = ref(false)
const matchingClusters = computed(() => {
  const query = clusterQuery.value.trim().toLowerCase()
  return clusters.value.filter(c => c.name.toLowerCase().includes(query))
})

const CONVERSATIONS_KEY = 'aegisops_conversations_collapsed'
const conversationsCollapsed = ref(localStorage.getItem(CONVERSATIONS_KEY) === '1')
const mobileDrawerOpen = ref(false)
watch(conversationsCollapsed, (collapsed) => {
  localStorage.setItem(CONVERSATIONS_KEY, collapsed ? '1' : '0')
})

const MENTION_RE = /@([a-zA-Z0-9][\w.-]*)/g

/**
 * 从输入中剥离已识别的 `@技能名` 引用：
 * - 命中的名称进入 skills 列表（后端强校验启停）；
 * - 未知 @ 串原样保留在查询中，避免误伤普通输入。
 */
function extractSkillMentions(
  query: string,
  skills: Skill[],
): { cleaned: string; names: string[] } {
  const names: string[] = []
  const spans: Array<[number, number]> = []
  for (const match of query.matchAll(MENTION_RE)) {
    const alias = match[1]
    const skill = skills.find((s) => s.name.toLowerCase() === alias.toLowerCase())
    if (!skill) {
      continue
    }
    if (!names.some((n) => n.toLowerCase() === skill.name.toLowerCase())) {
      names.push(skill.name)
    }
    spans.push([match.index, match.index + match[0].length])
  }
  if (spans.length === 0) {
    return { cleaned: query, names }
  }
  const parts: string[] = []
  let cursor = 0
  for (const [start, end] of spans) {
    parts.push(query.slice(cursor, start))
    cursor = end
  }
  parts.push(query.slice(cursor))
  return { cleaned: parts.join('').replace(/\s+/g, ' ').trim(), names }
}

const route = useRoute()
const router = useRouter()

const activeSession = computed(
  () => sessions.value.find((session) => session.id === activeId.value) ?? null,
)

/** 会话列表/审批卡展示集群名：ID 是稳定绑定，名称只是可读标签。 */
const clusterLabels = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const cluster of clusters.value) {
    map[cluster.id] = cluster.name || cluster.id
  }
  return map
})

/** 当前会话绑定的集群（不是新会话目标选择器）。目标失效时明确标注，不回落。 */
const activeClusterName = computed(() => {
  const id = activeSession.value?.clusterId
  if (!id) return ''
  return clusterLabels.value[id] || `目标不可用（${id}）`
})

/** 当前会话创建者：管理员跨用户查看时也必须能明确看到归属。 */
const activeOwnerName = computed(() => {
  const sess = activeSession.value
  if (!sess) return ''
  if (sess.ownerName) return sess.ownerName
  if (sess.ownerId && sess.ownerId === auth.user?.userId) {
    return auth.user?.displayName || auth.user?.username || sess.ownerId
  }
  return sess.ownerId || ''
})

/** 仅用于提示“正在查看他人会话”，不代表审批或操作权限。 */
const viewingOthersSession = computed(
  () =>
    Boolean(auth.user?.userId) &&
    Boolean(activeSession.value?.ownerId) &&
    activeSession.value?.ownerId !== auth.user?.userId,
)

const showScopeWelcome = computed(() => {
  if (!activeSession.value || activeSession.value.messageCount > 0) {
    return false
  }
  return stream.turns.value.length === 0 && stream.items.value.length === 0
})

const scopeWelcome = computed(() => {
  const c=clusters.value.find(c=>c.id===activeSession.value?.clusterId)
  return {title:c?'授权集群：'+c.name:'没有可用的集群授权',hint:'集群对象与命名空间分别授权，写操作需要确认。',tags:c?.access.rules.map(r=>(r.scopeType==='cluster'?'集群对象':r.namespaces?.join('、'))+'：'+r.actions.join('、'))??[]}

})

/** P2.9 审批操作显隐：只读角色不展示审批按钮（后端仍独立强校验）。 */
const canDecide = computed(() => canDecideApproval(auth.role) && activeSession.value?.ownerId===auth.user?.userId)

/** P3.8.4 @ 技能引用权限：仅具备 skill.read 时开启提示与回填。 */
const canUseSkills = computed(() => hasPermission(permissionsOf(auth.role), PERM.skillRead))

/** 会话级 Token：规范 Turn 用量的累计（覆盖语义，不重复相加）。 */
const sessionToken = computed(() => stream.token.value)

const statusText = computed(() => {
  switch (stream.status.value) {
    case 'connecting':
      return '连接中'
    case 'open':
      return '已连接'
    case 'reconnecting':
      return `重连中（第 ${stream.reconnectAttempt.value} 次）`
    case 'closed':
    case 'idle':
    default:
      return '未连接'
  }
})

/** P7.6 流状态胶囊 tone：对齐全局 .state-pill.tone-*，替代 Element Plus 的扁平 tag。 */
function streamTone(status: StreamStatus): 'running' | 'done' | 'waiting' | 'idle' {
  switch (status) {
    case 'connecting':
      return 'running'
    case 'open':
      return 'done'
    case 'reconnecting':
      return 'waiting'
    case 'closed':
    case 'idle':
    default:
      return 'idle'
  }
}

async function loadSessions(): Promise<void> {
  loading.value = true
  try {
    sessions.value = await listSessions()
    if (activeId.value && !sessions.value.some((session) => session.id === activeId.value)) {
      activeId.value = null
      sessionStorage.removeItem(ACTIVE_SESSION_KEY)
    }
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '加载会话失败')
  } finally {
    loading.value = false
  }
}



function selectSession(id: string): void {
  mobileDrawerOpen.value = false
  if (id === activeId.value) {
    // 同一会话重复点击按「重新订阅」处理：冷启动或断流后点一下就恢复，避免点击无反应。
    if (stream.sessionId.value === id) {
      void stream.reconnect()
    } else {
      void stream.connect(id)
    }
    return
  }
  activeId.value = id
  sessionStorage.setItem(ACTIVE_SESSION_KEY, id)
}

async function handleBranch(turnId: string) {
  const source = activeId.value
  if (!source || viewingOthersSession.value || branching.value || sending.value || stream.running.value) return
  branching.value = true
  try {
    await ElMessageBox.confirm('仅继承截至此回复的历史问答，不继承审批、执行状态或工具输出。新会话不会自动执行操作。', '开启新会话')
    if (activeId.value !== source) return
    if (pendingBranch?.sessionId !== source || pendingBranch?.turnId !== turnId) {
      pendingBranch = { sessionId: source, turnId, key: newIdempotencyKey() }
    }
    const result = await branchSession(source, turnId, pendingBranch.key)
    pendingBranch = undefined
    await loadSessions()
    selectSession(result.sessionId)
    ElMessage.success('已开启分支会话')
  } catch (error) {
    if (error !== 'cancel' && error !== 'close') {
      ElMessage.error(error instanceof Error ? error.message : '创建分支失败')
    }
  } finally {
    branching.value = false
  }
}

function openBranchSource() {
  const origin = branchOrigin.value
  if (!origin) return
  if (!sessions.value.some(session => session.id === origin.sessionId && session.state !== 'restricted')) {
    ElMessage.warning('来源会话不可访问')
    return
  }
  selectSession(origin.sessionId)
  const stop = watch(
    () => stream.turns.value.some(turn => turn.session_id === origin.sessionId && turn.turn_id === origin.turnId),
    (ready) => {
      if (ready) {
        document.getElementById(`turn-${origin.turnId}`)?.scrollIntoView({ block: 'center' })
        stop()
      }
    }, { flush: 'post' },
  )
  window.setTimeout(stop, 5000)
}

function handleReconnect(): void {
  if (!activeId.value) {
    return
  }
  if (stream.sessionId.value === activeId.value) {
    void stream.reconnect()
  } else {
    void stream.connect(activeId.value)
  }
}

async function createForCluster(id: string): Promise<void> {
  if (creatingSession.value) return
  creatingSession.value = true
  try {
    await loadScope()
    if (!clusters.value.some(c => c.id === id)) { ElMessage.warning('该集群不可用或无权访问'); return }
    const session = await createSession({clusterId:id})
    showClusterPicker.value = false
    mobileDrawerOpen.value = false
    activeId.value = session.id
    sessionStorage.setItem(ACTIVE_SESSION_KEY, session.id)
    await loadSessions()
    await nextTick()
    composerRef.value?.focus()
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '创建会话失败')
  } finally {
    creatingSession.value = false
  }
}

async function handleCreate(): Promise<void> {
  await loadScope()
  if (clusters.value.length === 0) { ElMessage.warning('没有可用集群，请联系管理员'); return }
  if (clusters.value.length === 1) { await createForCluster(clusters.value[0]!.id); return }
  clusterQuery.value = ''
  showClusterPicker.value = true
}

async function handleRename(payload: { id: string; name: string }): Promise<void> {
  try {
    await renameSession(payload.id, payload.name)
    await loadSessions()
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '重命名会话失败')
  }
}

async function handlePin(payload: { id: string; pinned: boolean }): Promise<void> {
  try {
    await pinSession(payload.id, payload.pinned)
    await loadSessions()
    ElMessage.success(payload.pinned ? '已置顶' : '已取消置顶')
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '置顶操作失败')
  }
}

/** P7.5：导出当前会话为 Markdown 文件（前端 Blob 下载，不新增后端接口）。 */
async function handleExport(target: SessionSummary): Promise<void> {
  try {
    const detail = await createSession({ id: target.id })
    const markdown = buildSessionMarkdown(detail.messages ?? [], {
      id: detail.id,
      name: detail.name ?? target.name,
      model: detail.model,
    })
    const blob = new Blob([markdown], { type: 'text/markdown;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = markdownFileName(detail.name ?? target.name)
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '导出会话失败')
  }
}

async function handleDelete(target: SessionSummary): Promise<void> {
  try {
    await ElMessageBox.confirm(`确定删除会话「${target.name || target.id}」？`, '删除会话', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await deleteSession(target.id)
    if (activeId.value === target.id) {
      activeId.value = null
      sessionStorage.removeItem(ACTIVE_SESSION_KEY)
    }
    await loadSessions()
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '删除会话失败')
  }
}

/** 请求幂等键：同一动作的网络重试复用同一个 turn，不创建第二轮。 */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `req-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

async function handleSend(): Promise<void> {
  const raw = input.value.trim()
  if (!activeId.value || !raw || sending.value || stream.running.value) {
    return
  }
  const available = composerRef.value?.availableSkills ?? []
  const { cleaned, names } = extractSkillMentions(raw, available)
  sending.value = true
  input.value = ''
  try {
    await sendMessage(activeId.value, {
      query: cleaned || raw,
      modelId: nextModel.value || undefined,
      skills: names,
      idempotencyKey: newIdempotencyKey(),
    })
  } catch (error) {
    sending.value = false
    if (!input.value) input.value = raw
    if (nextModel.value) void refreshModels()
    ElMessage.error(error instanceof Error ? error.message : '发送失败')
  }
}

/** 结构化回答回传（问题卡）；选择只表达业务决策，不授予写权限。 */
function handleAnswer(payload: {
  interactionId: string
  status: 'selected' | 'text' | 'skipped'
  optionId?: string
  text?: string
}): void {
  const turnId = stream.pending.value?.turn_id ?? ''
  if (!activeId.value || !turnId) {
    return
  }
  void answerInteraction(activeId.value, {
    interactionId: payload.interactionId,
    turnId,
    status: payload.status,
    optionId: payload.optionId,
    text: payload.text,
  }).catch((error) => {
    ElMessage.error(error instanceof Error ? error.message : '提交回答失败')
  })
}

/** 停止当前正在执行的会话运行。 */
function handleStop(): void {
  const id = activeId.value
  if (!id || stoppingSession.value === id) {
    return
  }
  stoppingSession.value = id
  void cancelRun(id, AbortSignal.timeout(10_000)).catch((error) => {
    if (stoppingSession.value !== id) return
    stoppingSession.value = ''
    if (activeId.value !== id) return
    void stream.resync()
    ElMessage.warning(error instanceof Error && error.name !== 'TimeoutError'
      ? error.message
      : '暂未确认停止，正在同步会话')
  })
}

watch([stoppingSession, activeId], ([id, active], _, onCleanup) => {
  if (!id || id !== active) return
  // 终态事件丢失时重取快照。仅在服务端确认终态后恢复发送。
  const syncTimer = window.setTimeout(() => void stream.resync(), 5_000)
  const settleTimer = window.setTimeout(() => {
    stoppingSession.value = ''
    ElMessage.warning('暂未确认停止，可重试停止或重新同步会话')
  }, 15_000)
  onCleanup(() => {
    window.clearTimeout(syncTimer)
    window.clearTimeout(settleTimer)
  })
})

/** 审批回传：把用户选择回传给服务端。 */
function handleApprove(value: string,id?:string): void {
  if (!activeId.value) {
    return
  }
  void approveRun(activeId.value, value,id, stream.pending.value?.revision).catch((error) => {
    ElMessage.error(error instanceof Error ? error.message : '审批回传失败')
  })
}

/** 审批拒绝：与旧 UI 一致，回传 `no` 作为否定选择。 */
function handleReject(id?:string): void {
  if (!activeId.value) {
    return
  }
  void approveRun(activeId.value, 'no',id, stream.pending.value?.revision).catch((error) => {
    ElMessage.error(error instanceof Error ? error.message : '拒绝回传失败')
  })
}

/**
 * P12.4 集群工作台入口：`/app/chat?cluster=<id>&new=1` 表示“为这个目标新建
 * 一个会话”。这里只设置新会话目标并调用新建，不改写任何已有会话的绑定；
 * 目标不可用时给出明确提示，不回落其它集群。
 */
/**
 * P4.7 审计中心 / 报告页的会话入口：`/app/chat?session=<id>` 表示直接打开
 * 该会话。这里只负责切换；能不能看得见仍由后端按 owner / admin 判定
 * （越权与不存在同形 404，页面收到错误就诚实提示，不回落其它会话）。
 */
async function applySessionEntry(): Promise<void> {
  if (!route) {
    return
  }
  const id = route.query.session
  if (typeof id !== 'string' || !id) {
    return
  }
  const query = { ...route.query }
  delete query.session
  await router.replace({ query })
  if (!sessions.value.some((session) => session.id === id)) {
    ElMessage.warning('该会话不存在或你无权查看')
    return
  }
  activeId.value = id
}

async function applyClusterEntry(): Promise<void> {
  if (!route) {
    return
  }
  const id = route.query.cluster
  if (typeof id !== 'string' || !id) {
    return
  }
  const createNew = route.query.new === '1'
  if (createNew) {
    const query = { ...route.query }
    delete query.new
    delete query.cluster
    await router.replace({ query })
  }
  const available = clusters.value.find((cluster) => cluster.id === id)
  if (!available) {
    ElMessage.warning('该集群未接入或你无权访问；未创建会话（不会自动改用其它集群）')
    return
  }
  if (createNew) {
    await createForCluster(id)
  }
}

onMounted(async () => {
  void refreshModels()
  await loadSessions()
  await loadScope()
  await applySessionEntry()
  await applyClusterEntry()
})

// 会话切换：先由 connect 内部 clear() 中止旧连接，再订阅新会话。
// immediate 不可省：activeId 从 sessionStorage 恢复时首帧不会触发 watch，
// 缺了它刷新后不会订阅 SSE（页面停在「未连接 / 暂无消息」）。
watch(
  activeId,
  (id) => {
    stoppingSession.value = ''
    branchOrigin.value = null
    if (id) {
      void getBranchOrigin(id).then(origin => {
        if (activeId.value === id && origin?.sessionId && origin.turnId) branchOrigin.value = origin
      }).catch(() => undefined)
      sending.value = false
      void stream.connect(id)
    } else {
      stream.stop()
      sending.value = false
    }
  },
  { immediate: true },
)

// 运行状态只由规范 Turn 状态驱动：出现终态即复位发送态并刷新列表。
watch(
  () => stream.running.value,
  (running) => {
    if (!running && stoppingSession.value === activeId.value) stoppingSession.value = ''
    if (!running && sending.value) {
      sending.value = false
      void loadSessions()
    }
  },
)

// protocol 升级（426）或 fatal：提示用户手动重新同步，不静默重试。
watch(
  () => stream.state.value.fatal,
  (fatal) => {
    if (fatal) {
      sending.value = false
      ElMessage.error(fatal.message || '输出未能可靠保存，请重新同步')
    }
  },
)
</script>

<template>
  <section class="chat-page" :class="{ 'ctx-collapsed': conversationsCollapsed }">
    <el-dialog v-model="showClusterPicker" title="选择问诊集群" width="420px" append-to-body>
      <p class="cluster-dialog-hint">选择后创建新会话，已有会话仍绑定原集群。</p>
      <el-input v-model="clusterQuery" placeholder="搜索集群" clearable aria-label="搜索集群" />
      <div class="cluster-dialog-list">
        <button v-for="cluster in matchingClusters" :key="cluster.id" class="cluster-dialog-option" type="button" :disabled="creatingSession" @click="createForCluster(cluster.id)">
          {{ cluster.name }} <span>开始问诊 ›</span>
        </button>
        <p v-if="!matchingClusters.length">没有匹配的集群</p>
      </div>
    </el-dialog>
    <ConversationList
      class="chat-side"
      :class="{ 'drawer-open': mobileDrawerOpen }"
      :sessions="sessions"
      :active-id="activeId"
      :cluster-names="clusterLabels"
      :current-user-id="auth.user?.userId"
      :loading="loading"
      :collapsed="conversationsCollapsed"
      @create="handleCreate"
      @select="selectSession"
      @delete="handleDelete"
      @rename="handleRename"
      @pin="handlePin"
      @export="handleExport"
      @refresh="loadSessions"
      @toggle="conversationsCollapsed = !conversationsCollapsed"
    />
    <div
      v-if="mobileDrawerOpen"
      class="mobile-backdrop"
      aria-hidden="true"
      @click="mobileDrawerOpen = false"
    />
    <main class="chat-main">
      <header class="chat-head">
        <el-button
          class="mobile-menu-btn"
          circle
          plain
          size="small"
          :icon="ChatDotRound"
          aria-label="打开会话列表"
          @click="mobileDrawerOpen = true"
        />
        <div class="chat-heading">
          <h2>{{ activeSession?.name || '未选择会话' }}</h2>
          <span v-if="activeSession" class="chat-id" :title="activeSession.id">
            {{ activeSession.id }}
          </span>
        </div>
        <div v-if="activeSession" class="chat-meta">
          <DiagnosticMemories v-if="!viewingOthersSession" :cluster-id="activeSession.clusterId"
            :referenced-ids="contextBudget?.memory_ids" />
          <el-button v-if="branchOrigin" text size="small" @click="openBranchSource">继承历史问答 · 查看来源</el-button>
          <span class="cluster-badge" :title="activeSession.clusterId || ''">
            目标集群：{{ activeClusterName || '未绑定' }}
          </span>
          <span class="owner-badge" :title="activeOwnerName">
            创建者：{{ activeOwnerName || '未知' }}
          </span>
          <span v-if="viewingOthersSession" class="state-pill tone-waiting" title="管理员查看他人会话，操作仍按平台权限与目标授权判定">
            他人会话
          </span>
          <span class="state-pill" :class="`tone-${sessionStateTone(activeSession.state)}`">
            {{ activeSession.state }}
          </span>
        </div>
      </header>
      <div class="chat-body">
        <template v-if="activeSession">
          <main class="chat-stream">
            <header class="stream-bar">
              <div class="stream-state">
                <span class="state-pill" :class="`tone-${streamTone(stream.status.value)}`">
                  {{ statusText }}
                </span>
                <span v-if="stream.lastFrameAt.value" class="stream-last">
                  最后事件更新于 {{ new Date(stream.lastFrameAt.value).toLocaleTimeString() }}
                </span>
              </div>
              <div class="stream-actions">
                <el-button size="small" text :icon="RefreshRight" @click="handleReconnect">
                  重连
                </el-button>
              </div>
            </header>
            <p v-if="stream.error.value && !stream.state.value.fatal" class="stream-error">{{ stream.error.value }}</p>
            <div v-if="stream.state.value.fatal" class="stream-error" role="alert">
              <span>{{ stream.state.value.fatal.message || '输出未能可靠保存，请重新同步' }}</span>
              <el-button size="small" text type="danger" @click="stream.resync()">重新同步</el-button>
            </div>
            <section v-if="showScopeWelcome && scopeWelcome" class="scope-welcome" aria-label="当前授权范围">
              <div class="scope-welcome-icon">AUTH</div>
              <div class="scope-welcome-copy">
                <strong>{{ scopeWelcome.title }}</strong>
                <p>{{ scopeWelcome.hint }}</p>
                <div v-if="scopeWelcome.tags.length" class="scope-tags">
                  <span v-for="tag in scopeWelcome.tags" :key="tag" class="scope-tag">{{ tag }}</span>
                </div>
              </div>
            </section>
            <MessageTimeline
              :turns="stream.turns.value"
              :question-index="stream.questions.value"
              :can-branch="!viewingOthersSession && !sending && !stream.running.value && !branching"
              @branch="handleBranch"
              :items="stream.items.value"
              :can-decide="canDecide"
              :cluster-name="activeClusterName"
              empty-title="该会话暂无消息"
              empty-hint="在下方输入框描述你的运维问题；Agent 会先检查现场，再给出可复核的结论"
              @approve="handleApprove"
              @reject="handleReject"
              @answer="handleAnswer"
            />
          </main>
        </template>
        <template v-else>
          <TechEmptyState
            title="未选择会话"
            hint="新建一个会话，描述你的运维问题；Agent 会先思考、再调用工具核实现场"
          >
            <el-button type="primary" :icon="ChatDotRound" @click="handleCreate"
              >新建会话</el-button
            >
          </TechEmptyState>
        </template>
      </div>

      <footer class="chat-composer">
        <ComposerBox
          ref="composerRef"
          v-model="input"
          :model="nextModel || activeSession?.model || ''"
          :models="modelOptions"
          :model-labels="modelLabels"
          @select-model="selectedModel = $event"
          :disabled="!activeSession"
          :sending="sending || stream.running.value"
          :stopping="stoppingSession === activeId"
          :can-use-skills="canUseSkills"
          @send="handleSend"
          @stop="handleStop"
        />
        <div v-if="activeSession" class="chat-budget">
          <TaskStatusBar
            v-if="latestTools.length"
            :key="latestTurn?.turn_id"
            :tools="latestTools"
            :turn="latestTurn"
            :pending="stream.pending.value"
            :stopping="stoppingSession === activeId"
          />
          <span v-else-if="runStatus" class="chat-task-status" role="status">{{ runStatus }}</span>
          <TokenUsageBadge :token="sessionToken" />
          <el-tooltip :content="contextDetails" placement="top" :trigger="['hover', 'focus', 'click']">
            <button type="button" class="chat-budget-button">
              <span class="chat-budget-ring" aria-hidden="true"
                :style="{ '--budget': `${Math.min(100, contextPercent ?? 0)}%` }" />
              {{ stream.running.value ? '上下文预算' : '上下文预算（上次请求）' }} ·
              {{ contextPercent === null ? '—' : `约 ${Math.round(contextPercent)}%` }}
            </button>
          </el-tooltip>
        </div>
      </footer>
    </main>
  </section>
</template>

<style scoped>
.chat-budget {
  position: relative;
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  align-items: center;
  gap: 4px 10px;
  padding-top: 4px;
}
.chat-task-status {
  color: var(--app-muted);
  font-size: 12px;
}
.chat-budget-button {
  display: inline-flex;
  gap: 6px;
  align-items: center;
  background: transparent;
  border: 0;
  color: var(--app-muted);
  font-size: 12px;
  cursor: help;
}
.chat-budget-ring {
  width: 14px;
  height: 14px;
  border-radius: 50%;
  background: conic-gradient(currentColor var(--budget), var(--app-border) 0);
  mask: radial-gradient(transparent 48%, black 53%);
}
.cluster-dialog-hint { color: var(--app-muted); margin: 0 0 14px; }
.cluster-dialog-list { display: flex; flex-direction: column; gap: 8px; max-height: 320px; overflow-y: auto; margin-top: 14px; }
.cluster-dialog-option { display: flex; justify-content: space-between; gap: 12px; width: 100%; padding: 12px; border: 1px solid var(--app-hairline); border-radius: 8px; color: var(--app-text); background: var(--app-card-bg); cursor: pointer; text-align: left; }
.cluster-dialog-option:hover { border-color: var(--app-accent); }
.cluster-dialog-option span { color: var(--app-accent); white-space: nowrap; }
.chat-page {
  display: grid;
  /* 对齐原型 .chat-pane：会话栏 240px + 16px 间距；收起态 56px */
  grid-template-columns: 240px minmax(0, 1fr);
  /* 行必须显式填满容器，否则隐式 auto 行按内容高度撑开，把输入框顶出视口 */
  grid-template-rows: minmax(0, 1fr);
  gap: 16px;
  /* P7.2 经典双栏：由 .app-main（flex 列）约束为"占满可用高度"，消息再多也不把输入框顶下去 */
  flex: 1 1 0;
  min-height: 0;
  overflow: hidden;
  transition: grid-template-columns 0.2s ease;
}

.chat-page.ctx-collapsed {
  grid-template-columns: 56px minmax(0, 1fr);
}

.chat-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
}

.chat-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 0 4px 10px;
}

.chat-heading {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.chat-heading h2 {
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 15px;
  font-weight: 600;
}

.chat-id {
  max-width: 160px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  padding: 0 8px;
  border: 1px solid var(--app-hairline);
  border-radius: 6px;
  background: var(--app-neutral-soft);
  color: var(--app-muted);
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 11px;
  line-height: 18px;
}

.chat-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px 10px;
  flex: none;
}

.chat-count {
  font-size: 12px;
  color: var(--app-muted);
}

.target-picker {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
}

.target-label {
  font-size: 11.5px;
  color: var(--app-muted);
  white-space: nowrap;
}

/* 当前会话绑定目标：与“新会话目标”选择器视觉分离，避免误以为切换器会迁移会话。 */
.cluster-badge,
.owner-badge {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  padding: 2px 9px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
}

.cluster-badge {
  border: 1px solid var(--app-accent-border);
  background: var(--app-accent-soft);
  color: var(--app-accent-2);
}

.owner-badge {
  max-width: 150px;
  border: 1px solid var(--app-border);
  background: var(--app-surface-2);
  color: var(--app-muted);
}

.chat-body {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
  /* P7.6 海拔：画布比侧栏/会话列表亮一档 + 微弱内发光，解决"中间一大块黑" */
  background: var(--app-canvas);
  box-shadow: var(--app-canvas-inset);
  border: 1px solid var(--app-hairline);
  border-radius: var(--app-radius);
}

.chat-stream {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
}

.stream-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 0 2px 8px;
}

.stream-state {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.stream-state .state-pill {
  font-variant: tabular-nums;
}

.stream-last {
  font-size: 12px;
  color: var(--app-faint);
}

.stream-actions {
  flex: none;
}

/* P7.6 断流提示：软色玻璃条 + 左侧高亮条 + 微弱光晕，替代原来的纯色块 */
.stream-error {
  display: flex;
  align-items: center;
  margin: 0 0 8px;
  padding: 7px 12px;
  border: 1px solid var(--app-danger-border);
  border-left: 2px solid var(--app-danger);
  border-radius: var(--app-radius-sm);
  background: var(--app-danger-soft);
  box-shadow: 0 0 18px -8px var(--app-danger);
  color: var(--app-danger);
  font-size: 12px;
}

.scope-welcome {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  flex: none;
  margin: 4px 0 12px;
  padding: 14px 16px;
  border: 1px solid var(--app-accent-border);
  border-radius: var(--app-radius);
  background:
    linear-gradient(135deg, rgba(99, 102, 241, 0.1), transparent 62%),
    var(--app-surface-2);
  box-shadow: 0 0 28px -18px var(--app-accent);
}

.scope-welcome-icon {
  flex: none;
  padding: 4px 7px;
  border: 1px solid var(--app-accent-border);
  border-radius: 6px;
  color: var(--app-accent-2);
  font-family: var(--app-font-mono, monospace);
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 0.08em;
}

.scope-welcome-copy {
  min-width: 0;
}

.scope-welcome-copy strong {
  display: block;
  margin-bottom: 5px;
  color: var(--app-strong);
  font-size: 13px;
}

.scope-welcome-copy p {
  margin: 0;
  color: var(--app-muted);
  font-size: 12px;
  line-height: 1.7;
}

.scope-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 9px;
}

.scope-tag {
  padding: 2px 8px;
  border: 1px solid rgba(34, 211, 238, 0.26);
  border-radius: 999px;
  background: rgba(34, 211, 238, 0.08);
  color: var(--app-accent-2);
  font-family: var(--app-font-mono, monospace);
  font-size: 11px;
}

.chat-composer {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-top: 12px;
}

/* P7.5：移动端（≤768px）会话列表改为抽屉，默认收起、顶栏按钮唤出。 */
.mobile-menu-btn {
  display: none;
  flex: none;
}

.mobile-backdrop {
  display: none;
}

@media (max-width: 768px) {
  .chat-page,
  .chat-page.ctx-collapsed {
    grid-template-columns: minmax(0, 1fr);
  }

  .mobile-menu-btn {
    display: inline-flex;
  }

  .chat-side {
    position: fixed;
    top: 8px;
    bottom: 8px;
    left: 8px;
    width: 300px;
    max-width: 86vw;
    z-index: 50;
    transform: translateX(-110%);
    transition: transform 0.22s ease;
  }

  .chat-side.drawer-open {
    transform: translateX(0);
  }

  .mobile-backdrop {
    display: block;
    position: fixed;
    inset: 0;
    z-index: 40;
    background: rgba(0, 0, 0, 0.45);
    backdrop-filter: blur(2px);
  }
}
</style>
