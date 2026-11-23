<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { listAuditEvents, auditStatusLabel } from '@/api/audit'
import { listClusters } from '@/api/clusters'
import type { AuditEventView, Cluster } from '@/api/types'
import {
  describeReportError,
  previewReportRetention,
  listReportRetentionPolicies,
  saveReportRetentionPolicy,
  type ReportRetentionPolicy,
  type ReportRetentionPreview,
} from '@/api/reports'

const route = useRoute()
const clusters = ref<Cluster[]>([])
const runs = ref<AuditEventView[]>([])
const retentionPolicies = ref<ReportRetentionPolicy[]>([])
const retentionLoading = ref(false)
const retentionResult = ref<ReportRetentionPreview | null>(null)
const retentionCluster = ref(typeof route.query.clusterId === 'string' ? route.query.clusterId : '')
const retentionKind = ref(route.query.kind === 'latest-json' ? 'latest-json' : 'latest-md')
const retentionDays = ref(90)
const retentionMinimum = ref(5)
const retentionInterval = ref(60)

async function loadRuns() {
  try {
    runs.value = (await listAuditEvents({ action: 'report.retention.run', limit: 20 })).items
  } catch (err) {
    ElMessage.error(describeReportError('读取清理记录', err))
  }
}

async function loadPolicies() {
  try {
    retentionPolicies.value = await listReportRetentionPolicies()
  } catch (err) {
    ElMessage.error(describeReportError('读取保留策略', err))
  }
}

async function changeRetentionPolicy(enabled: boolean, saved?: ReportRetentionPolicy) {
  if (retentionLoading.value || (enabled && !retentionResult.value)) return
  const preview = retentionResult.value ?? undefined
  const policy = saved ?? {
    clusterId: retentionCluster.value,
    kind: retentionKind.value,
    days: retentionDays.value,
    minimum: retentionMinimum.value,
    intervalMinutes: retentionInterval.value,
  }
  retentionLoading.value = true
  try {
    if (enabled) {
      try {
        await ElMessageBox.confirm(
          '启用后将定时删除符合策略的报告引用，范围包含所选集群和类型下各用户的报告。',
          '启用报告自动清理',
          { confirmButtonText: '启用', type: 'warning' },
        )
      } catch {
        return
      }
    }
    await saveReportRetentionPolicy({ ...policy, enabled }, preview)
    retentionResult.value = null
    ElMessage.success(enabled ? '已启用报告自动清理' : '已停用报告自动清理')
    retentionPolicies.value = await listReportRetentionPolicies()
  } catch (err) {
    ElMessage.error(describeReportError('更新保留策略', err))
  } finally {
    retentionLoading.value = false
  }
}

async function loadRetentionPreview() {
  if (retentionLoading.value) return
  retentionLoading.value = true
  retentionResult.value = null
  try {
    retentionResult.value = await previewReportRetention(
      retentionCluster.value,
      retentionKind.value,
      retentionDays.value,
      retentionMinimum.value,
      retentionInterval.value,
    )
  } catch (err) {
    ElMessage.error(describeReportError('计算保留预览', err))
  } finally {
    retentionLoading.value = false
  }
}


onMounted(async () => {
  void loadPolicies()
  void loadRuns()
  try {
    clusters.value = await listClusters()
  } catch (err) {
    ElMessage.error(describeReportError('读取集群', err))
  }
})
</script>

<template>
  <section class="card">
    <h2>报告保留</h2>
      <el-form label-position="top" :disabled="retentionLoading" @submit.prevent="loadRetentionPreview">
        <el-form-item label="集群">
          <el-select v-model="retentionCluster" filterable @change="retentionResult = null">
            <el-option v-for="cluster in clusters" :key="cluster.id" :label="cluster.name" :value="cluster.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="报告类型">
          <el-select v-model="retentionKind" @change="retentionResult = null">
            <el-option label="Markdown" value="latest-md" />
            <el-option label="JSON" value="latest-json" />
          </el-select>
        </el-form-item>
        <el-form-item label="保留天数">
          <el-input-number v-model="retentionDays" :min="1" :max="36500" :precision="0" @change="retentionResult = null" />
        </el-form-item>
        <el-form-item label="至少保留最近份数">
          <el-input-number v-model="retentionMinimum" :min="1" :max="1000000" :precision="0" @change="retentionResult = null" />
        </el-form-item>
        <el-form-item label="执行周期">
          <el-select v-model="retentionInterval" @change="retentionResult = null">
            <el-option v-for="minutes in [15, 30, 60, 360, 1440]" :key="minutes"
              :value="minutes" :label="`${minutes} 分钟`" />
          </el-select>
        </el-form-item>
        <el-button native-type="submit" type="primary" :loading="retentionLoading"
          :disabled="!retentionCluster || !retentionDays || !retentionMinimum">
          计算预览
        </el-button>
      </el-form>
      <div v-if="retentionResult" aria-live="polite">
        <p>共 {{ retentionResult.total }} 份，按策略保护 {{ retentionResult.protected }} 份，可清理 {{ retentionResult.eligible }} 份，无法确认扫描终态 {{ retentionResult.excluded }} 份。</p>
        <el-button type="danger" :disabled="retentionLoading" @click="changeRetentionPolicy(true)">
          启用自动清理
        </el-button>
        <p>到期截止时间 {{ retentionResult.cutoff }}</p>
        <p>包含该集群和报告类型下各用户的报告。扫描终态无法确认的报告会保留。</p>
      </div>
      <div v-for="policy in retentionPolicies" :key="policy.clusterId + ':' + policy.kind">
        <p>
          {{ clusters.find(cluster => cluster.id === policy.clusterId)?.name || '集群名称不可用' }}
          · {{ policy.kind === 'latest-md' ? 'Markdown' : 'JSON' }}
          · {{ policy.enabled ? '已启用' : '已停用' }}
          · {{ policy.days }} 天 / 至少 {{ policy.minimum }} 份 / 每 {{ policy.intervalMinutes }} 分钟
        </p>
        <p v-if="policy.enabled">下次执行 {{ new Date(policy.nextRun).toLocaleString() }}</p>
        <p v-if="policy.lastRun">上次清理 {{ new Date(policy.lastRun).toLocaleString() }}，删除 {{ policy.lastDeleted }} 份</p>
        <el-button v-if="policy.enabled" size="small" :disabled="retentionLoading"
          @click="changeRetentionPolicy(false, policy)">停用</el-button>
      </div>
    <h3>最近运行</h3>
    <el-button text @click="loadRuns">刷新运行记录</el-button>
    <el-table :data="runs">
      <el-table-column label="执行时间">
        <template #default="{ row }">{{ new Date(row.timestamp).toLocaleString() }}</template>
      </el-table-column>
      <el-table-column label="集群">
        <template #default="{ row }">{{ clusters.find(cluster => cluster.id === row.clusterId)?.name || '集群名称不可用' }}</template>
      </el-table-column>
      <el-table-column label="报告类型">
        <template #default="{ row }">{{ row.resourceName === 'latest-md' ? 'Markdown' : 'JSON' }}</template>
      </el-table-column>
      <el-table-column label="状态">
        <template #default="{ row }">{{ auditStatusLabel(row.status) }}</template>
      </el-table-column>
      <el-table-column prop="resultSummary" label="执行结果" />
    </el-table>
  </section>
</template>
