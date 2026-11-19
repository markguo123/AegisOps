<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteModelProfile, describeSettingsError, listModelProfiles, saveModelProfile } from '@/api/settings'
import type { ModelProfile, ModelProfileInput } from '@/api/settings'

defineProps<{ canWrite: boolean }>()

const profiles = ref<ModelProfile[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const dialogOpen = ref(false)
const editing = ref<ModelProfile>()
const clearKey = ref(false)

function emptyDraft(): ModelProfileInput {
  return { name: '', model: '', endpoint: '', apiFormat: 'openai-chat-completions', contextWindow: 65536, maxTokens: 4096, apiKey: '' }
}

const draft = ref(emptyDraft())
const ready = computed(() => draft.value.name.trim() && draft.value.model.trim() && draft.value.endpoint.trim() &&
  draft.value.contextWindow >= 8192 && draft.value.maxTokens > 0 && draft.value.maxTokens < draft.value.contextWindow)

async function load() {
  loading.value = true
  error.value = ''
  try {
    profiles.value = await listModelProfiles()
  } catch (failure) {
    error.value = describeSettingsError('读取独立模型', failure)
  } finally {
    loading.value = false
  }
}

function open(profile?: ModelProfile) {
  editing.value = profile
  clearKey.value = false
  if (profile) {
    const { name, model, endpoint, apiFormat, contextWindow, maxTokens } = profile
    draft.value = { name, model, endpoint, apiFormat, contextWindow, maxTokens, apiKey: '' }
  } else {
    draft.value = emptyDraft()
  }
  dialogOpen.value = true
}

async function save() {
  if (!ready.value || saving.value) return
  saving.value = true
  try {
    const { apiKey, ...input } = draft.value
    await saveModelProfile({
      ...input,
      ...(clearKey.value ? { apiKey: '' } : apiKey?.trim() ? { apiKey } : {}),
    }, editing.value?.id)
    dialogOpen.value = false
    await load()
    ElMessage.success('模型已保存')
  } catch (failure) {
    ElMessage.error(describeSettingsError('保存模型', failure))
  } finally {
    saving.value = false
  }
}

async function remove(profile: ModelProfile) {
  try {
    await ElMessageBox.confirm(`删除「${profile.name}」后，Chat 无法再选择该配置。`, '删除模型')
    await deleteModelProfile(profile.id)
    await load()
  } catch (failure) {
    if (failure !== 'cancel' && failure !== 'close') ElMessage.error(describeSettingsError('删除模型', failure))
  }
}

onMounted(load)
</script>

<template>
  <section class="model-profiles">
    <header>
      <div>
        <strong>独立问诊模型</strong>
        <p>每个模型单独配置接口与凭据，保存后可在 Chat 中切换。</p>
      </div>
      <el-button v-if="canWrite" type="primary" @click="open()">添加模型</el-button>
    </header>
    <p v-if="loading">正在加载…</p>
    <p v-else-if="error" role="alert">{{ error }}</p>
    <p v-else-if="!profiles.length">尚未添加独立模型。</p>
    <ul v-else>
      <li v-for="profile in profiles" :key="profile.id">
        <div>
          <strong>{{ profile.name }}</strong>
          <p>{{ profile.model }} · {{ profile.contextWindow.toLocaleString() }} Token</p>
        </div>
        <div v-if="canWrite">
          <el-button text @click="open(profile)">编辑</el-button>
          <el-button text type="danger" @click="remove(profile)">删除</el-button>
        </div>
      </li>
    </ul>

    <el-dialog v-model="dialogOpen" :title="editing ? '编辑模型' : '添加模型'" width="min(520px, 92vw)" @closed="draft = emptyDraft()">
      <el-form label-position="top" @submit.prevent="save">
        <el-form-item label="显示名称"><el-input v-model="draft.name" maxlength="128" /></el-form-item>
        <el-form-item label="模型 ID"><el-input v-model="draft.model" maxlength="256" /></el-form-item>
        <el-form-item label="接口地址"><el-input v-model="draft.endpoint" placeholder="https://gateway.example/v1" /></el-form-item>
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
      </el-form>
      <template #footer>
        <el-button @click="dialogOpen = false">取消</el-button>
        <el-button type="primary" :disabled="!ready" :loading="saving" @click="save">保存模型</el-button>
      </template>
    </el-dialog>
  </section>
</template>

<style scoped>
.model-profiles {
  display: grid;
  gap: 8px;
  padding: 12px;
  border: 1px solid var(--app-border);
  border-radius: 8px;
}
header, li {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}
ul {
  list-style: none;
  padding: 0;
  margin: 0;
}
li {
  padding: 8px 0;
  border-top: 1px solid var(--app-border);
  overflow-wrap: anywhere;
}
p {
  margin: 4px 0 0;
  color: var(--app-muted);
  font-size: 12px;
}
</style>
