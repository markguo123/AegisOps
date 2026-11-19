<script setup lang="ts">
import { computed, ref, watch, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import { checkModel, describeSettingsError, saveModelProfile, type ModelProfile, type ModelProfileInput } from '@/api/settings'
const props = defineProps<{ editing?: ModelProfile }>()
const emit = defineEmits<{ close: []; saved: [id: string, status: string] }>()
const saving = ref(false)
const clearKey = ref(false)

function emptyDraft(): ModelProfileInput {
  return { name: '', model: '', endpoint: '', apiFormat: 'openai-chat-completions', contextWindow: 65536, maxTokens: 4096, apiKey: '' }
}

const draft = ref(emptyDraft())
const ready = computed(() => draft.value.name.trim() && draft.value.model.trim() && draft.value.endpoint.trim() &&
  draft.value.contextWindow >= 8192 && draft.value.maxTokens > 0 && draft.value.maxTokens < draft.value.contextWindow)

const candidates = ref<string[]>([])
const busy = ref('')
const notice = ref('')
const probe = ref('')
let controller: AbortController | undefined
onUnmounted(() => { revision++; controller?.abort() })
let revision = 0

watch([() => draft.value.endpoint, () => draft.value.apiKey, clearKey, () => draft.value.model], () => {
  controller?.abort()
  revision++
  busy.value = ''
  notice.value = ''
  probe.value = ''
}, { flush: 'sync' })
watch([() => draft.value.endpoint, () => draft.value.apiKey, clearKey], () => { candidates.value = [] })

async function check(action: 'discover' | 'probe') {
  const current = ++revision
  controller = new AbortController()
  busy.value = action
  notice.value = ''
  const { model, endpoint, apiFormat, apiKey } = draft.value
  try {
    const result = await checkModel(action, { profileId: props.editing?.id, model, endpoint, apiFormat,
      apiKey: clearKey.value ? '' : apiKey || undefined }, controller.signal)
    if (revision !== current) return
    if (action === 'discover') {
      candidates.value = result.models
      notice.value = `获取到 ${result.models.length} 个候选模型`
    } else {
      probe.value = '测试通过 · ' + new Date(result.checkedAt).toLocaleTimeString()
      notice.value = probe.value
    }
  } catch (failure) {
    if (revision !== current) return
    notice.value = describeSettingsError('模型检测', failure)
    if (action === 'probe') probe.value = '测试失败 · ' + new Date().toLocaleTimeString()
  } finally {
    if (revision === current) busy.value = ''
  }
}

if (props.editing) {
    const { name, model, endpoint, apiFormat, contextWindow, maxTokens } = props.editing
    draft.value = { name, model, endpoint, apiFormat, contextWindow, maxTokens, apiKey: '' }
}
async function save() {
  if (!ready.value || saving.value) return
  saving.value = true
  try {
    const { apiKey, ...input } = draft.value
    const result = await saveModelProfile({
      ...input,
      ...(clearKey.value ? { apiKey: '' } : apiKey?.trim() ? { apiKey } : {}),
    }, props.editing?.id)
    if (result.data) emit('saved', result.data.id, probe.value)
    ElMessage.success('模型已保存')
  } catch (failure) {
    ElMessage.error(describeSettingsError('保存模型', failure))
  } finally {
    saving.value = false
  }
}

</script>
<template>
    <el-dialog :model-value="true" :close-on-click-modal="!saving" :close-on-press-escape="!saving" :show-close="!saving" @close="emit('close')" :title="editing ? '编辑模型' : '添加模型'" width="min(520px, 92vw)">
      <el-form :disabled="saving" label-position="top" @submit.prevent="save">
        <el-form-item label="显示名称"><el-input v-model="draft.name" maxlength="128" /></el-form-item>
        <el-form-item label="模型 ID"><el-select v-if="candidates.length" v-model="draft.model" filterable allow-create default-first-option>
          <el-option v-for="id in candidates" :key="id" :label="id" :value="id" />
        </el-select><el-input v-else v-model="draft.model" maxlength="256" /></el-form-item>
        <el-form-item label="接口地址"><el-input v-model="draft.endpoint" placeholder="https://gateway.example/v1" /></el-form-item>
        <el-form-item label="Provider">OpenAI 兼容</el-form-item>
        <el-form-item label="API 格式">
          <el-select v-model="draft.apiFormat">
            <el-option value="openai-chat-completions" label="OpenAI 兼容 · Chat Completions" />
          </el-select>
        </el-form-item>
        <el-form-item label="API Key">
          <el-input v-model="draft.apiKey" type="password" :disabled="clearKey"
            :placeholder="editing?.hasKey ? '留空保留已保存的密钥' : '无鉴权时可留空'" />
          <el-checkbox v-if="editing?.hasKey" v-model="clearKey">清除已保存的密钥</el-checkbox>
        </el-form-item>
        <el-form-item label="上下文总窗口（Token）">
          <el-input-number v-model="draft.contextWindow" :min="8192" :max="4194304" :precision="0" />
        </el-form-item>
        <el-form-item label="单次输出上限（Token）">
          <el-input-number v-model="draft.maxTokens" :min="1" :precision="0" />
        </el-form-item>
        <el-button :loading="busy === 'discover'" :disabled="!!busy || !draft.endpoint" @click="check('discover')">获取候选模型</el-button>
        <el-button :loading="busy === 'probe'" :disabled="!!busy || !draft.endpoint || !draft.model" @click="check('probe')">测试所选模型</el-button>
        <p role="status">{{ notice }}</p>
      </el-form>
      <template #footer>
        <el-button :disabled="saving" @click="emit('close')">取消</el-button>
        <el-button type="primary" :disabled="!ready || !!busy" :loading="saving" @click="save">保存模型</el-button>
      </template>
    </el-dialog>
</template>
