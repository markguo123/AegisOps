<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteModelProfile, describeSettingsError, listModelProfiles } from '@/api/settings'
import type { ModelProfile } from '@/api/settings'

import ModelProfileDialog from './ModelProfileDialog.vue'

defineProps<{ canWrite: boolean }>()

const profiles = ref<ModelProfile[]>([])
const loading = ref(false)
const error = ref('')
const dialogOpen = ref(false)
const editing = ref<ModelProfile>()
const statuses = ref<Record<string, string>>({})
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
  dialogOpen.value = true
}

async function saved(id: string, status: string) {
  statuses.value[id] = status
  dialogOpen.value = false
  await load()
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
        <strong>问诊模型</strong>
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
          <p>OpenAI 兼容 · {{ profile.model }} · {{ profile.contextWindow.toLocaleString() }} Token</p>
        </div>
        <small :class="{ ok: statuses[profile.id]?.startsWith('测试通过'), failed: statuses[profile.id]?.startsWith('测试失败') }">{{ statuses[profile.id] || '未测试' }}</small>
        <div v-if="canWrite">
          <el-button text @click="open(profile)">编辑</el-button>
          <el-button text type="danger" @click="remove(profile)">删除</el-button>
        </div>
      </li>
    </ul>

    <ModelProfileDialog v-if="dialogOpen" :editing="editing" @close="dialogOpen = false" @saved="saved" />
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
small::before { content: "●"; margin-right: 5px; color: var(--app-muted); }
small.ok::before { color: #26986b; }
small.failed::before { color: #d65353; }
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
