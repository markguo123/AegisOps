<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { getQuestion, answerQuestion, type UserQuestion } from '@/api/sessions'

const props = defineProps<{ sessionId: string | null; revision?: number; running?: boolean }>()
const question = ref<UserQuestion | null>(null)
const selected = ref('')
const text = ref('')
const custom = ref(false)
const busy = ref(false)
const error = ref('')
let generation = 0
async function refresh() {
  const id = props.sessionId
  const version = generation
  if (!id || busy.value) return
  try {
    const q = await getQuestion(id)
    if (version !== generation || id !== props.sessionId) return
    if (q?.id !== question.value?.id) {
      selected.value = ''; text.value = ''; custom.value = false; error.value = ''
    }
    question.value = q
  } catch { if (version === generation) question.value = null }
}
watch(() => props.revision, () => { void refresh() })
watch(() => props.sessionId, () => { generation++; question.value = null; void refresh() }, { immediate: true })
const timer = setInterval(() => { if (props.running || question.value) void refresh() }, 1500)
onBeforeUnmount(() => { generation++; clearInterval(timer) })
async function submit(skip = false) {
  const q = question.value
  if (!q || busy.value) return
  busy.value = true; error.value = ''; generation++
  try {
    await answerQuestion(q, skip ? 'skipped' : custom.value ? 'text' : 'selected', custom.value ? '' : selected.value, custom.value ? text.value : '')
    if (question.value?.id === q.id) question.value = null
  } catch (e) { error.value = e instanceof Error ? e.message : '提交失败，请重试' }
  finally { busy.value = false }
}
</script>

<template>
  <section v-if="question" class="question-card" aria-label="待回答的问题">
    <strong>{{ question.question }}</strong>
    <label v-for="option in question.options" :key="option.id" class="question-option">
      <input v-model="selected" type="radio" :value="option.id" :disabled="busy" @change="custom = false">
      <span>{{ option.label }}{{ option.recommended ? '（推荐）' : '' }}<small>{{ option.impact }}</small></span>
    </label>
    <label><input v-model="custom" type="checkbox" :disabled="busy"> 自由输入</label>
    <textarea v-if="custom" v-model="text" maxlength="2000" :disabled="busy" placeholder="请输入你的选择，不要填写密码或 Secret" />
    <p v-if="error" role="alert">{{ error }}</p>
    <div class="question-actions">
      <button :disabled="busy" @click="submit(true)">跳过</button>
      <button :disabled="busy || (custom ? !text.trim() : !selected)" @click="submit()">{{ busy ? '提交中…' : '提交选择' }}</button>
    </div>
  </section>
</template>

<style scoped>
.question-card { padding: 16px; margin-bottom: 12px; border: 1px solid var(--el-border-color); border-radius: 10px; max-height: 400px; overflow: auto; }
.question-option { display: flex; gap: 10px; margin: 12px 0; cursor: pointer; }
small { display: block; color: var(--el-text-color-secondary); margin-top: 4px; }
textarea { display: block; width: 100%; min-height: 70px; margin: 8px 0; }
.question-actions { display: flex; justify-content: flex-end; gap: 12px; margin-top: 12px; }
</style>


