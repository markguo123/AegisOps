<script setup lang="ts">
import { ref } from 'vue'
import type { DiagnosticMemoryDraft } from './diagnosticMemoryDraft'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiDelete, apiGet, apiPatch, apiPost } from '@/api/request'

const props = defineProps<{
  clusterId?: string
  clusters: ReadonlyArray<{ id: string; name: string }>
  namespace?: string
  referencedIds?: string[]
}>()

function clusterName(id?: string): string {
  if (!id) return '未绑定集群'
  return props.clusters.find(cluster => cluster.id === id)?.name || '集群名称不可用'
}

interface Memory {
  id?: string
  version?: string
  requestKey?: string
  expiresAt?: string
  clusterId?: string
  namespace?: string
  relatedNamespaces?: string[]
  workload?: string
  text?: string
  unavailable?: boolean
}

const open = ref(false)
const busy = ref(false)
const rows = ref<Memory[]>([])
const draft = ref<Memory | null>(null)
const related = ref('')

async function run(action: () => Promise<void>) {
  busy.value = true
  try {
    await action()
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '诊断记忆操作失败')
  } finally {
    busy.value = false
  }
}

async function refresh() {
  rows.value = (await apiGet<Memory[]>('/me/memories')).data ?? []
}

function show() {
  open.value = true
  draft.value = null
  rows.value = []
  void run(refresh)
}

function edit(memory?: Memory) {
  draft.value = memory ? { ...memory } : {
    clusterId: props.clusterId,
    namespace: props.namespace,
    requestKey: Array.from(crypto.getRandomValues(new Uint32Array(4))).join('-'),
    workload: '',
    text: '',
  }
  related.value = memory?.relatedNamespaces?.join(', ') ?? ''
}

function reviewSuggestion(suggestion?: DiagnosticMemoryDraft) {
  show()
  edit()
  if (suggestion && draft.value) {
    Object.assign(draft.value, suggestion)
    related.value = suggestion.relatedNamespaces.join(', ')
  }
}

defineExpose({ reviewSuggestion })

async function save() {
  const memory = draft.value
  if (!memory || busy.value) return
  await run(async () => {
    const input = { ...memory, relatedNamespaces: related.value.split(/[,，\s]+/).filter(Boolean) }
    if (memory.id) await apiPatch('/me/memories/' + encodeURIComponent(memory.id), input)
    else await apiPost('/me/memories', input)
    draft.value = null
    await refresh()
  })
}

async function forget(id?: string) {
  if (!id || busy.value) return
  try {
    await ElMessageBox.confirm('遗忘后，后续问诊将不再召回这条经验。', '遗忘诊断记忆')
  } catch {
    return
  }
  await run(async () => {
    await apiDelete('/me/memories/' + encodeURIComponent(id))
    if (draft.value?.id === id) draft.value = null
    await refresh()
  })
}
</script>

<template>
  <el-button size="small" @click="show">诊断记忆</el-button>
  <el-button v-if="referencedIds?.length" size="small" link @click="show">
    参考了 {{ referencedIds.length }} 条诊断记忆
  </el-button>
  <el-dialog v-model="open" title="我的诊断记忆" width="min(580px, 92vw)" append-to-body>
    <div v-loading="busy">
      <el-button v-if="clusterId" :disabled="busy" @click="edit()">记住排查经验</el-button>
      <p v-if="!rows.length && !busy">暂无诊断记忆</p>
      <p v-if="!busy && referencedIds?.some(id => !rows.some(memory => memory.id === id))">
        部分引用已不可用
      </p>
      <div v-for="memory in rows" :key="memory.id">
        <el-tag v-if="memory.id && referencedIds?.includes(memory.id)" size="small">本轮参考</el-tag>
        <template v-if="memory.unavailable">
          <p>当前范围不可用</p>
        </template>
        <template v-else>
          <p>{{ clusterName(memory.clusterId) }} / {{ memory.namespace }} / {{ memory.workload }}</p>
          <p>{{ memory.text }}</p>
          <el-button size="small" :disabled="busy" @click="edit(memory)">编辑</el-button>
        </template>
        <el-button size="small" :disabled="busy" @click="forget(memory.id)">遗忘</el-button>
      </div>
      <el-form v-if="draft" label-position="top" :disabled="busy" @submit.prevent="save">
        <el-form-item label="集群"><el-input :model-value="clusterName(draft.clusterId)" readonly /></el-form-item>
        <el-form-item label="主命名空间"><el-input v-model="draft.namespace" /></el-form-item>
        <el-form-item label="关联命名空间（逗号分隔）"><el-input v-model="related" /></el-form-item>
        <el-form-item label="工作负载名称"><el-input v-model="draft.workload" /></el-form-item>
        <el-form-item label="排查经验">
          <el-input v-model="draft.text" type="textarea" maxlength="500" show-word-limit />
        </el-form-item>
        <el-form-item label="到期时间（可选）">
          <el-date-picker v-model="draft.expiresAt" type="datetime" value-format="YYYY-MM-DDTHH:mm:ssZ" />
        </el-form-item>
        <p>匹配时，排查经验会发送给当前对话所选模型。</p>
        <el-button @click="draft = null">取消</el-button>
        <el-button native-type="submit" type="primary" :loading="busy"
          :disabled="!draft.namespace || !draft.workload || !draft.text">确认保存</el-button>
      </el-form>
    </div>
  </el-dialog>
</template>
