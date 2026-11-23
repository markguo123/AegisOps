<script setup lang="ts">
/**
 * P12.4 多集群工作台。
 *
 * 口径：
 *  - 顶部统计与列表都来自 `/clusters/workspace`：后端已按当前用户授权过滤，
 *    检查时间/状态来自最近一次连测；前端只做展示、搜索和状态筛选。
 *  - "连接成功"不等于"具备全部业务权限"；成功、失败、未检查、已过期分开标注。
 *  - 所有管理动作（连测/更新凭据/删除/接入）只对有 cluster.manage 的角色出现，
 *    后端每个请求仍逐次核权。
 *  - 问诊/体检/报告入口携带明确 ClusterID 跳到对应页面，由目标页面创建新会话
 *    或预选目标；不会改写已有会话的绑定。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Link, Refresh, Search, Setting, View } from '@element-plus/icons-vue'

import {
  checkCluster,
  clusterContexts,
  clusterWorkspace,
  createCluster,
  deleteCluster,
  describeClusterError,
  testCluster,
  updateCluster,
} from '@/api/clusters'
import type {
  ClusterContexts,
  ClusterInfo,
  ClusterWorkspace,
  ClusterWorkspaceItem,
} from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { hasPermission, PERM, permissionsOf } from '@/utils/permission'

const router = useRouter()
const auth = useAuthStore()

const workspace = ref<ClusterWorkspace | null>(null)
const loading = ref(false)
const keyword = ref('')
const statusFilter = ref('')
const detailOpen = ref(false)
const detailItem = ref<ClusterWorkspaceItem | null>(null)

const canManage = computed(() =>
  hasPermission(permissionsOf(auth.role), PERM.clusterManage),
)

const items = computed(() => workspace.value?.items ?? [])

const filteredItems = computed(() => {
  const query = keyword.value.trim().toLowerCase()
  return items.value.filter((item) => {
    if (statusFilter.value && item.checkState !== statusFilter.value) {
      return false
    }
    if (!query) {
      return true
    }
    const c = item.cluster
    return (
      c.name.toLowerCase().includes(query) ||
      (c.context ?? '').toLowerCase().includes(query) ||
      (c.server ?? '').toLowerCase().includes(query)
    )
  })
})

const summary = computed(
  () => workspace.value?.summary ?? { total: 0, connected: 0, failed: 0, unchecked: 0 },
)

const generatedAt = computed(() =>
  workspace.value?.generatedAt ? formatTime(workspace.value.generatedAt) : '—',
)

function formatTime(value?: string | null): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

function safeCheckSummary(summary?: string): string {
  if (summary?.includes('凭据有效')) return 'API Server 可达；凭据有效性及业务资源权限未完全验证'
  return summary || ''
}

function checkStateLabel(item: ClusterWorkspaceItem): string {
  switch (item.checkState) {
    case 'ok':
      return '连测成功'
    case 'failed':
      return '连测失败'
    case 'stale':
      return '结果已过期'
    default:
      return '未检查'
  }
}

function checkStateTone(item: ClusterWorkspaceItem): 'ok' | 'err' | 'warn' | 'mute' {
  switch (item.checkState) {
    case 'ok':
      return 'ok'
    case 'failed':
      return 'err'
    case 'stale':
      return 'warn'
    default:
      return 'mute'
  }
}

/** 授权范围展示：不把规则拼成可执行承诺，只说明当前用户能看/能操作哪一段。 */
function accessText(item: ClusterWorkspaceItem): string {
  if (!item.access.length) {
    return '无有效授权'
  }
  return item.access
    .map((rule) => {
      const scope =
        rule.scopeType === 'cluster' ? '集群对象' : (rule.namespaces ?? []).join('、') || '—'
      return `${scope}：${rule.actions.join('、')}`
    })
    .join('；')
}

function updateDetailFromWorkspace(): void {
  if (!detailOpen.value || !detailItem.value) return
  const fresh = items.value.find((item) => item.cluster.id === detailItem.value?.cluster.id)
  detailItem.value = fresh ?? null
  if (!fresh) detailOpen.value = false
}

async function loadWorkspace(): Promise<void> {
  loading.value = true
  try {
    workspace.value = await clusterWorkspace()
    updateDetailFromWorkspace()
  } catch (error) {
    ElMessage.error(describeClusterError('加载集群工作台', error))
    workspace.value = null
  } finally {
    loading.value = false
  }
}

function openDetail(item: ClusterWorkspaceItem): void {
  detailItem.value = item
  detailOpen.value = true
}

async function handleCheck(item: ClusterWorkspaceItem): Promise<void> {
  checkingId.value = item.cluster.id
  try {
    const view = await checkCluster(item.cluster.id)
    if (view.cluster.status === 'connected') {
      ElMessage.success(`「${view.cluster.name}」连测成功（不代表已具备全部业务权限）`)
    } else {
      ElMessage.warning(
        `「${view.cluster.name}」连测失败：${view.cluster.checkSummary || '原因未知'}`,
      )
    }
    await loadWorkspace()
  } catch (error) {
    ElMessage.error(describeClusterError('连测', error))
  } finally {
    checkingId.value = ''
  }
}

const checkingId = ref('')

async function handleDelete(item: ClusterWorkspaceItem): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `确定删除集群「${item.cluster.name}」及其加密凭据？已绑定该集群的会话、报告和审批不会自动迁移。`,
      '删除集群',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await deleteCluster(item.cluster.id)
    ElMessage.success('集群已删除')
    detailOpen.value = false
    await loadWorkspace()
  } catch (error) {
    ElMessage.error(describeClusterError('删除集群', error))
  }
}

function goChat(item: ClusterWorkspaceItem): void {
  if (!item.canChat) {
    ElMessage.warning('当前账号对该集群没有可用授权，无法发起问诊')
    return
  }
  void router.push({ name: 'chat', query: { cluster: item.cluster.id, new: '1' } })
}

function goScan(item: ClusterWorkspaceItem): void {
  if (!item.canScan) {
    ElMessage.warning('当前账号没有发起体检的权限（scan.execute）')
    return
  }
  void router.push({ name: 'report', query: { cluster: item.cluster.id, scan: '1' } })
}

function goReports(item: ClusterWorkspaceItem): void {
  void router.push({ name: 'report', query: { cluster: item.cluster.id } })
}

// ---- 接入抽屉：导入 → 选择明确 context → 连测 → 保存 ----
const connectOpen = ref(false)
const connectName = ref('')
const connectKubeconfig = ref('')
const connectParsed = ref<ClusterContexts | null>(null)
const connectContext = ref('')
const connectTest = ref<ClusterInfo | null>(null)
const connectError = ref('')
const connectTesting = ref(false)
const connectSaving = ref(false)

watch(connectKubeconfig, () => {
  connectParsed.value = null
  connectContext.value = ''
  connectTest.value = null
  connectError.value = ''
})

function openConnect(): void {
  connectOpen.value = true
  connectError.value = ''
}

function resetConnect(): void {
  connectName.value = ''
  connectKubeconfig.value = ''
  connectParsed.value = null
  connectContext.value = ''
  connectTest.value = null
  connectError.value = ''
}

async function parseConnectContexts(): Promise<boolean> {
  const kubeconfig = connectKubeconfig.value.trim()
  if (!kubeconfig) {
    connectError.value = '请先粘贴或上传 kubeconfig'
    return false
  }
  try {
    const parsed = await clusterContexts(kubeconfig)
    connectParsed.value = parsed
    connectContext.value = parsed.current || parsed.contexts[0] || ''
    connectError.value = ''
    return true
  } catch (error) {
    connectError.value = describeClusterError('解析 kubeconfig', error)
    return false
  }
}

async function handleConnectTest(): Promise<void> {
  if (!connectParsed.value && !(await parseConnectContexts())) {
    return
  }
  connectTesting.value = true
  connectTest.value = null
  connectError.value = ''
  try {
    connectTest.value = await testCluster({
      kubeconfig: connectKubeconfig.value.trim(),
      context: connectContext.value,
    })
    ElMessage.success('连接测试通过')
  } catch (error) {
    connectError.value = describeClusterError('连接测试', error)
    ElMessage.error(connectError.value)
  } finally {
    connectTesting.value = false
  }
}

async function handleCreate(): Promise<void> {
  if (!connectParsed.value && !(await parseConnectContexts())) {
    return
  }
  connectSaving.value = true
  try {
    await createCluster({
      name: connectName.value.trim() || undefined,
      context: connectContext.value,
      kubeconfig: connectKubeconfig.value.trim(),
    })
    ElMessage.success('集群已接入，凭据已加密存储')
    connectOpen.value = false
    resetConnect()
    await loadWorkspace()
  } catch (error) {
    connectError.value = describeClusterError('保存集群', error)
    ElMessage.error(connectError.value)
  } finally {
    connectSaving.value = false
  }
}

function onConnectFile(event: Event): void {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  const reader = new FileReader()
  reader.onload = () => {
    connectKubeconfig.value = String(reader.result ?? '')
    void parseConnectContexts()
  }
  reader.onerror = () => ElMessage.error('读取 kubeconfig 文件失败')
  reader.readAsText(file)
  input.value = ''
}

// ---- 凭据更新抽屉：候选先连测 → 身份一致才允许保存 ----
const updateOpen = ref(false)
const updateItem = ref<ClusterWorkspaceItem | null>(null)
const updateKubeconfig = ref('')
const updateParsed = ref<ClusterContexts | null>(null)
const updateContext = ref('')
const updateTest = ref<ClusterInfo | null>(null)
const updateError = ref('')
const updateTesting = ref(false)
const updateSaving = ref(false)
const updateConfirmServer = ref(false)
const updateServerChanged = ref(false)

function openUpdate(item: ClusterWorkspaceItem): void {
  updateItem.value = item
  updateKubeconfig.value = ''
  updateParsed.value = null
  updateContext.value = ''
  updateTest.value = null
  updateError.value = ''
  updateConfirmServer.value = false
  updateServerChanged.value = false
  updateOpen.value = true
}

watch(updateKubeconfig, () => {
  updateParsed.value = null
  updateContext.value = ''
  updateTest.value = null
  updateError.value = ''
  updateServerChanged.value = false
  updateConfirmServer.value = false
})

async function parseUpdateContexts(): Promise<boolean> {
  const kubeconfig = updateKubeconfig.value.trim()
  if (!kubeconfig) {
    updateError.value = '请先粘贴或上传新的 kubeconfig'
    return false
  }
  try {
    const parsed = await clusterContexts(kubeconfig)
    updateParsed.value = parsed
    updateContext.value = parsed.current || parsed.contexts[0] || ''
    updateError.value = ''
    return true
  } catch (error) {
    updateError.value = describeClusterError('解析 kubeconfig', error)
    return false
  }
}

async function handleUpdateTest(): Promise<boolean> {
  if (!updateParsed.value && !(await parseUpdateContexts())) {
    return false
  }
  updateTesting.value = true
  updateTest.value = null
  updateError.value = ''
  try {
    updateTest.value = await testCluster({
      kubeconfig: updateKubeconfig.value.trim(),
      context: updateContext.value,
    })
    updateServerChanged.value =
      Boolean(updateTest.value.server) &&
      Boolean(updateItem.value?.cluster.server) &&
      updateTest.value.server !== updateItem.value?.cluster.server
    ElMessage.success('候选凭据连接测试通过')
    return true
  } catch (error) {
    updateError.value = describeClusterError('候选凭据连测', error)
    ElMessage.error(updateError.value)
    return false
  } finally {
    updateTesting.value = false
  }
}

async function handleUpdate(): Promise<void> {
  if (!updateItem.value) return
  if (!updateTest.value && !(await handleUpdateTest())) {
    return
  }
  if (!updateTest.value) {
    updateError.value = '请先完成候选凭据连测'
    return
  }
  if (updateServerChanged.value && !updateConfirmServer.value) {
    updateError.value =
      '候选凭据的 API Server 地址与原目标不同，请核对后勾选“确认是同一集群的入口变更”'
    return
  }
  updateSaving.value = true
  try {
    await updateCluster(updateItem.value.cluster.id, {
      name: updateItem.value.cluster.name,
      context: updateContext.value,
      kubeconfig: updateKubeconfig.value.trim(),
      confirmServerChange: updateServerChanged.value ? updateConfirmServer.value : false,
    })
    ElMessage.success('凭据已原位更新；旧授权与待审批调用会因凭据版本变化失效')
    updateOpen.value = false
    await loadWorkspace()
  } catch (error) {
    updateError.value = describeClusterError('更新凭据', error)
    ElMessage.error(updateError.value)
  } finally {
    updateSaving.value = false
  }
}

function onUpdateFile(event: Event): void {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  const reader = new FileReader()
  reader.onload = () => {
    updateKubeconfig.value = String(reader.result ?? '')
    void parseUpdateContexts()
  }
  reader.onerror = () => ElMessage.error('读取 kubeconfig 文件失败')
  reader.readAsText(file)
  input.value = ''
}

onMounted(loadWorkspace)
</script>

<template>
  <section class="cluster-page">
    <div class="page-head">
      <div class="head-copy">
        <h2 class="title">集群工作台</h2>
        <span class="desc">
          {{ workspace?.dataSource || '平台受管集群注册表' }} · 数据截至 {{ generatedAt }}
        </span>
      </div>
      <el-button :icon="Refresh" :loading="loading" @click="loadWorkspace">刷新</el-button>
      <el-button v-if="canManage" type="primary" :icon="Link" @click="openConnect">
        ＋ 接入集群
      </el-button>
    </div>

    <div class="stat-grid">
      <div class="card stat">
        <div class="l">受管集群</div>
        <div class="v">{{ summary.total }}</div>
        <span class="u">当前用户可见的目标</span>
      </div>
      <div class="card stat">
        <div class="l">最近连测成功</div>
        <div class="v ok">{{ summary.connected }}</div>
        <span class="u">仅代表 API Server 可达；凭据和业务权限未完全验证</span>
      </div>
      <div class="card stat">
        <div class="l">最近连测失败</div>
        <div class="v err">{{ summary.failed }}</div>
        <span class="u">按目标隔离，不回落其它集群</span>
      </div>
      <div class="card stat">
        <div class="l">未检查</div>
        <div class="v">{{ summary.unchecked }}</div>
        <span class="u">没有连测时间的历史目标</span>
      </div>
    </div>

    <div class="card list-card">
      <div class="row list-head">
        <el-input
          v-model="keyword"
          class="search"
          size="small"
          clearable
          placeholder="搜索名称 / Context / Server"
          :prefix-icon="Search"
        />
        <el-select v-model="statusFilter" class="filter" size="small" placeholder="全部状态">
          <el-option label="全部状态" value="" />
          <el-option label="连测成功" value="ok" />
          <el-option label="连测失败" value="failed" />
          <el-option label="结果已过期" value="stale" />
          <el-option label="未检查" value="unchecked" />
        </el-select>
        <span class="pill mute">{{ filteredItems.length }} / {{ items.length }} 个</span>
      </div>

      <div v-if="loading" class="muted center-hint">加载中…</div>
      <div v-else-if="items.length === 0" class="muted center-hint">
        当前没有可用集群。管理员可点右上角「接入集群」；普通用户请联系管理员授予目标范围。
      </div>
      <div v-else-if="filteredItems.length === 0" class="muted center-hint">
        没有匹配的集群，试试清空搜索或状态筛选。
      </div>
      <div v-else class="cluster-list">
        <div v-for="item in filteredItems" :key="item.cluster.id" class="list-item">
          <div class="grow" @click="openDetail(item)">
            <div class="row">
              <span class="t">{{ item.cluster.name }}</span>
              <span class="pill" :class="checkStateTone(item)">{{ checkStateLabel(item) }}</span>
              <span v-if="!item.identityRecorded" class="pill warn" title="未记录身份依据，原位凭据更新会被拒绝">
                身份未记录
              </span>
            </div>
            <div class="muted meta">
              <span class="mono">{{ item.cluster.context || '无 Context' }}</span>
              <span class="faint">·</span>
              <span class="mono ellipsis">{{ item.cluster.server || '—' }}</span>
              <span class="faint">·</span>
              <span>检查于 {{ formatTime(item.cluster.checkedAt) }}</span>
            </div>
            <div v-if="item.cluster.checkSummary" class="check-reason">
              {{ safeCheckSummary(item.cluster.checkSummary) }}
            </div>
          </div>
          <div class="row actions">
            <el-button size="small" text :disabled="!item.canChat" @click="goChat(item)">问诊</el-button>
            <el-button size="small" text :disabled="!item.canScan" @click="goScan(item)">体检</el-button>
            <el-button size="small" text :disabled="!item.canViewReports" @click="goReports(item)">报告</el-button>
            <el-button size="small" text :icon="View" @click="openDetail(item)">详情</el-button>
            <el-button
              v-if="canManage"
              size="small"
              text
              :icon="Refresh"
              :loading="checkingId === item.cluster.id"
              @click="handleCheck(item)"
            >
              连测
            </el-button>
            <el-button
              v-if="canManage"
              size="small"
              text
              :icon="Setting"
              :disabled="!item.identityRecorded"
              :title="item.identityRecorded ? '' : '未记录集群身份依据，不能原位替换；请删除后重新接入并人工核对'"
              @click="openUpdate(item)"
            >
              更新凭据
            </el-button>
            <el-button
              v-if="canManage"
              size="small"
              text
              type="danger"
              :icon="Delete"
              @click="handleDelete(item)"
            >
              删除
            </el-button>
          </div>
        </div>
      </div>
    </div>

    <!-- 详情抽屉 -->
    <el-drawer v-model="detailOpen" title="集群详情" size="520px">
      <template v-if="detailItem">
        <div class="detail-head">
          <div>
            <h3>{{ detailItem.cluster.name }}</h3>
            <p class="mono muted">{{ detailItem.cluster.id }}</p>
          </div>
          <span class="pill" :class="checkStateTone(detailItem)">
            {{ checkStateLabel(detailItem) }}
          </span>
        </div>
        <dl class="kv">
          <dt>Context</dt>
          <dd class="mono">{{ detailItem.cluster.context || '—' }}</dd>
          <dt>API Server</dt>
          <dd class="mono">{{ detailItem.cluster.server || '—' }}</dd>
          <dt>Kubernetes 版本</dt>
          <dd class="mono">{{ detailItem.cluster.version || '—' }}</dd>
          <dt>最近连测</dt>
          <dd>{{ formatTime(detailItem.cluster.checkedAt) }}</dd>
          <dt>连测结论</dt>
          <dd>{{ safeCheckSummary(detailItem.cluster.checkSummary) || (detailItem.checkState === 'unchecked' ? '从未连测' : '—') }}</dd>
          <dt>集群身份依据</dt>
          <dd>{{ detailItem.identityRecorded ? '已记录（可验证同一逻辑集群）' : '未记录：原位替换凭据会被拒绝，请重新接入并人工核对' }}</dd>
          <dt>当前用户授权</dt>
          <dd>{{ accessText(detailItem) }}</dd>
        </dl>
        <p class="hint">
          连测仅确认 API Server 可达，凭据有效性及业务权限未完全验证；实际权限由目标集群 RBAC 与平台授权共同决定。
        </p>
        <div class="row drawer-actions">
          <el-button :disabled="!detailItem.canChat" @click="goChat(detailItem)">新建问诊会话</el-button>
          <el-button :disabled="!detailItem.canScan" @click="goScan(detailItem)">发起体检</el-button>
          <el-button :disabled="!detailItem.canViewReports" @click="goReports(detailItem)">查看报告</el-button>
        </div>
        <div v-if="canManage" class="row drawer-actions manage">
          <el-button :icon="Refresh" :loading="checkingId === detailItem.cluster.id" @click="handleCheck(detailItem)">
            立即连测
          </el-button>
          <el-button
            :icon="Setting"
            :disabled="!detailItem.identityRecorded"
            :title="detailItem.identityRecorded ? '' : '未记录集群身份依据，不能原位替换；请删除后重新接入并人工核对'"
            @click="openUpdate(detailItem)"
          >
            更新凭据
          </el-button>
          <el-button type="danger" plain :icon="Delete" @click="handleDelete(detailItem)">删除</el-button>
        </div>
      </template>
    </el-drawer>

    <!-- 接入抽屉 -->
    <el-drawer v-model="connectOpen" title="接入集群" size="560px" @closed="resetConnect">
      <p class="hint">
        导入 kubeconfig → 选择明确 context → 连测 → 保存。凭据只在提交时上传并加密存储，页面不缓存明文。
      </p>
      <label class="dropzone">
        <div class="big">⬆</div>
        拖拽或点击选择 kubeconfig
        <input type="file" accept=".yaml,.yml,.kubeconfig,text/*" class="file-input" @change="onConnectFile" />
        <span class="faint">文件内容仅加密后存储在服务器，页面不展示明文</span>
      </label>
      <el-input
        v-model="connectName"
        class="field"
        placeholder="集群名称（可选，默认取 Context）"
        clearable
      />
      <el-input
        v-model="connectKubeconfig"
        class="field"
        type="textarea"
        :rows="8"
        placeholder="在此粘贴 kubeconfig 内容"
        spellcheck="false"
      />
      <div class="row field-row">
        <el-select
          v-model="connectContext"
          class="grow"
          placeholder="选择 context"
          :disabled="!connectParsed"
        >
          <el-option v-for="ctx in connectParsed?.contexts ?? []" :key="ctx" :label="ctx" :value="ctx" />
        </el-select>
        <el-button :disabled="!connectKubeconfig.trim()" @click="parseConnectContexts">解析 Context</el-button>
      </div>
      <div v-if="connectTest" class="row result">
        <span class="pill ok">连接测试通过</span>
        <span class="mono muted ellipsis">
          {{ connectTest.version || '—' }} · {{ connectTest.server || '—' }}
        </span>
      </div>
      <p v-if="connectError" class="result-error">{{ connectError }}</p>
      <p class="hint">
        连测仅确认 API Server 可达，凭据有效性及业务资源权限未完全验证。
      </p>
      <div class="row actions end">
        <el-button :loading="connectTesting" @click="handleConnectTest">连接测试</el-button>
        <el-button
          type="primary"
          :loading="connectSaving"
          :disabled="!connectKubeconfig.trim() || !connectContext"
          @click="handleCreate"
        >
          保存连接
        </el-button>
      </div>
    </el-drawer>

    <!-- 凭据更新抽屉 -->
    <el-drawer v-model="updateOpen" title="更新集群凭据" size="560px">
      <template v-if="updateItem">
        <p class="hint">
          原位更新 {{ updateItem.cluster.name }}（{{ updateItem.cluster.id }}）。服务端会先连测候选凭据并核对集群身份；
          失败保留旧值。身份无法验证或指向另一集群时会拒绝，不能静默把其它集群替换进原 ClusterID。
        </p>
        <label class="dropzone">
          <div class="big">⬆</div>
          选择新的 kubeconfig
          <input type="file" accept=".yaml,.yml,.kubeconfig,text/*" class="file-input" @change="onUpdateFile" />
        </label>
        <el-input
          v-model="updateKubeconfig"
          class="field"
          type="textarea"
          :rows="8"
          placeholder="在此粘贴新的 kubeconfig 内容"
          spellcheck="false"
        />
        <div class="row field-row">
          <el-select
            v-model="updateContext"
            class="grow"
            placeholder="选择 context"
            :disabled="!updateParsed"
          >
            <el-option v-for="ctx in updateParsed?.contexts ?? []" :key="ctx" :label="ctx" :value="ctx" />
          </el-select>
          <el-button :disabled="!updateKubeconfig.trim()" @click="parseUpdateContexts">解析 Context</el-button>
        </div>
        <div v-if="updateTest" class="row result">
          <span class="pill ok">候选凭据连测通过</span>
          <span class="mono muted ellipsis">{{ updateTest.version || '—' }} · {{ updateTest.server || '—' }}</span>
        </div>
        <el-alert
          v-if="updateServerChanged"
          type="warning"
          :closable="false"
          show-icon
          title="候选凭据的 API Server 地址与已登记值不同"
        >
          <p>原目标：{{ updateItem.cluster.server || '—' }}</p>
          <p>新目标：{{ updateTest?.server || '—' }}</p>
          <el-checkbox v-model="updateConfirmServer">确认这是同一集群的入口变更</el-checkbox>
        </el-alert>
        <p v-if="updateError" class="result-error">{{ updateError }}</p>
        <div class="row actions end">
          <el-button :loading="updateTesting" @click="handleUpdateTest">连测候选凭据</el-button>
          <el-button type="primary" :loading="updateSaving" @click="handleUpdate">确认原位更新</el-button>
        </div>
      </template>
    </el-drawer>
  </section>
</template>

<style scoped>
.cluster-page {
  display: flex;
  flex-direction: column;
  gap: 14px;
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

.head-copy {
  min-width: 0;
  margin-right: auto;
}

.page-head .title {
  margin: 0;
  font-size: 16px;
}

.page-head .desc {
  display: block;
  margin-top: 4px;
  font-size: 12.5px;
  color: var(--app-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.stat-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
}

.card {
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
}

.stat {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 14px 16px;
  min-width: 0;
}

.stat .l {
  font-size: 12px;
  color: var(--app-muted);
}

.stat .v {
  font-size: 22px;
  font-weight: 650;
  color: var(--app-strong);
}

.stat .v.ok {
  color: var(--app-success);
}

.stat .v.err {
  color: var(--app-danger);
}

.stat .u {
  font-size: 11.5px;
  color: var(--app-faint);
}

.list-card {
  padding: 14px 16px;
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.list-head {
  padding-bottom: 10px;
  border-bottom: 1px solid var(--app-border);
}

.search {
  max-width: 320px;
}

.filter {
  width: 140px;
  margin-left: auto;
}

.cluster-list {
  display: flex;
  flex-direction: column;
}

.list-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 2px;
  border-bottom: 1px solid rgba(255, 255, 255, 0.05);
  min-width: 0;
}

.list-item:last-child {
  border-bottom: none;
}

.list-item .grow {
  cursor: pointer;
  min-width: 0;
}

.list-item .t {
  font-size: 13px;
  font-weight: 600;
  color: var(--app-text);
}

.meta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11.5px;
  margin-top: 3px;
  min-width: 0;
}

.check-reason {
  margin-top: 4px;
  font-size: 11.5px;
  color: var(--app-warning);
}

.actions {
  flex: none;
  flex-wrap: wrap;
  justify-content: flex-end;
}

.mono {
  font-family: var(--app-mono);
  font-size: 11.5px;
}

.muted {
  color: var(--app-muted);
}

.faint {
  color: var(--app-faint);
}

.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.grow {
  flex: 1;
  min-width: 0;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 20px;
  padding: 0 8px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
  white-space: nowrap;
}

.pill.ok {
  color: var(--app-success);
  background: var(--app-success-soft);
}

.pill.warn {
  color: var(--app-warning);
  background: var(--app-warning-soft);
}

.pill.err {
  color: var(--app-danger);
  background: var(--app-danger-soft);
}

.pill.mute {
  color: var(--app-muted);
  background: var(--app-surface-2);
}

.center-hint {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 120px;
  font-size: 12.5px;
  text-align: center;
  padding: 12px;
}

.detail-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--app-border);
}

.detail-head h3 {
  margin: 0;
  font-size: 15px;
}

.detail-head p {
  margin: 4px 0 0;
}

.kv {
  display: grid;
  grid-template-columns: 110px minmax(0, 1fr);
  gap: 8px 12px;
  margin: 16px 0;
  font-size: 12.5px;
}

.kv dt {
  color: var(--app-muted);
}

.kv dd {
  margin: 0;
  overflow-wrap: anywhere;
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--app-muted);
}

.drawer-actions {
  flex-wrap: wrap;
  margin-top: 12px;
}

.drawer-actions.manage {
  padding-top: 12px;
  border-top: 1px solid var(--app-border);
}

.dropzone {
  display: block;
  border: 1.5px dashed var(--app-border);
  border-radius: var(--app-radius);
  padding: 18px;
  text-align: center;
  color: var(--app-muted);
  font-size: 12.5px;
  cursor: pointer;
  margin-bottom: 12px;
}

.dropzone:hover {
  border-color: var(--app-accent-2);
  color: var(--app-text);
}

.dropzone .big {
  font-size: 22px;
  margin-bottom: 6px;
}

.dropzone .faint {
  display: block;
  margin-top: 6px;
  font-size: 11px;
}

.file-input {
  display: none;
}

.field {
  margin-bottom: 12px;
}

.field-row {
  margin-bottom: 12px;
}

.result {
  margin-bottom: 8px;
}

.result-error {
  margin: 0 0 8px;
  font-size: 12px;
  color: var(--app-danger);
}

.actions.end {
  justify-content: flex-end;
  margin-top: 12px;
}

@media (max-width: 900px) {
  .filter {
    margin-left: 0;
  }

  .list-item {
    align-items: flex-start;
    flex-direction: column;
  }

  .actions {
    justify-content: flex-start;
  }
}
</style>
