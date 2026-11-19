<script setup lang="ts">
import { CopyDocument, Share } from '@element-plus/icons-vue'
import copyText from '@/composables/useClipboard'

defineProps<{
  text: string
  canBranch?: boolean
  timestamp?: string
  timeLabel?: string
}>()

const emit = defineEmits<{ branch: [] }>()
</script>

<template>
  <div class="message-action-bar" role="group" aria-label="消息操作">
    <button type="button" aria-label="复制消息" title="复制消息" @click="copyText(text)">
      <CopyDocument aria-hidden="true" />
    </button>
    <button
      v-if="canBranch"
      type="button"
      aria-label="从这里开启新会话"
      title="从这里开启新会话"
      @click="emit('branch')"
    >
      <Share aria-hidden="true" />
    </button>
    <time v-if="timestamp && timeLabel" :datetime="timestamp" :title="timestamp">
      {{ timeLabel }}
    </time>
  </div>
</template>

<style scoped>
.message-action-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  min-height: 24px;
  color: var(--app-muted);
}

button {
  display: inline-grid;
  place-items: center;
  width: 24px;
  height: 24px;
  padding: 0;
  border: 0;
  border-radius: 4px;
  background: transparent;
  color: inherit;
  cursor: pointer;
}

button svg {
  width: 16px;
  height: 16px;
}

button:hover,
button:focus-visible {
  color: var(--app-text);
  background: var(--app-header-bg, var(--app-surface));
}

time {
  font-size: 11px;
  font-variant-numeric: tabular-nums;
  line-height: 1;
  white-space: nowrap;
}
</style>
