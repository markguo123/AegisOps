<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { revealSecret } from '@/api/sessions'
import copyText from '@/composables/useClipboard'

const props = defineProps<{ sessionId: string; params: unknown }>()
const values = ref<Record<string, { value: string; encoding: string }> | null>(null)
const busy = ref(false)
const error = ref('')
let request: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | undefined
const target = computed(() => {
  try {
    const data = typeof props.params === 'string' ? JSON.parse(props.params) : props.params
    if (
      !data ||
      typeof data.namespace !== 'string' ||
      typeof data.name !== 'string' ||
      !Array.isArray(data.keys)
    )
      return null
    return {
      namespace: data.namespace,
      name: data.name,
      keys: data.keys.filter((key: unknown): key is string => typeof key === 'string'),
    }
  } catch {
    return null
  }
})
function hide() {
  request?.abort()
  request = null
  clearTimeout(timer)
  values.value = null
  busy.value = false
}
async function show() {
  if (!target.value || busy.value) return
  hide()
  const controller = new AbortController()
  request = controller
  busy.value = true
  error.value = ''
  try {
    const result = await revealSecret(props.sessionId, target.value, controller.signal)
    if (controller.signal.aborted) return
    values.value = result.values
    timer = setTimeout(hide, 60_000)
  } catch (cause) {
    if (!controller.signal.aborted)
      error.value = cause instanceof Error ? cause.message : '读取失败'
  } finally {
    if (request === controller) busy.value = false
  }
}
watch(() => [props.sessionId, props.params], hide)
function onVisibilityChange() {
  if (document.hidden) hide()
}
window.addEventListener('blur', hide)
window.addEventListener('pagehide', hide)
document.addEventListener('visibilitychange', onVisibilityChange)
onBeforeUnmount(() => {
  hide()
  window.removeEventListener('blur', hide)
  window.removeEventListener('pagehide', hide)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <section v-if="target" class="secret-reveal">
    <span>Secret 值 · {{ target.namespace }}/{{ target.name }}</span>
    <button v-if="!values" type="button" :disabled="busy" @click="show">
      {{ busy ? '读取中…' : '显示明文' }}
    </button>
    <button v-else type="button" @click="hide">隐藏</button>
    <p v-if="error" role="alert">{{ error }}</p>
    <div v-for="(entry, key) in values" :key="key">
      <strong>{{ key }}</strong
      ><small v-if="entry.encoding === 'base64'"> 二进制 · Base64</small>
      <pre>{{ entry.value }}</pre>
      <button type="button" @click="copyText(entry.value, '已复制')">复制</button>
    </div>
    <small>仅当前页面显示，60 秒后自动隐藏，历史与审计不保存明文。</small>
  </section>
</template>

<style scoped>
.secret-reveal {
  padding: 12px;
  border-top: 1px solid var(--app-border);
  color: var(--app-text);
}
button {
  margin: 0 8px;
  cursor: pointer;
}
pre {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  user-select: text;
}
small {
  display: block;
  color: var(--app-muted);
  margin-top: 8px;
}
[role='alert'] {
  color: var(--app-danger);
}
</style>
