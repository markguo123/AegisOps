<script setup lang="ts">
import { computed, ref } from 'vue'
import type { TurnItem } from './turn'

const props = defineProps<{ items: TurnItem[]; completed: boolean; failed: boolean }>()
const manual = ref<boolean | null>(null)
const open = computed(() => manual.value ?? !props.completed)
const tools = computed(() => props.items.filter((item) => item.type === 'tool'))
const issues = computed(
  () =>
    tools.value.filter(
      (item) =>
        item.type === 'tool' && (item.data.status === 'error' || item.data.status === 'unknown'),
    ).length,
)
</script>

<template>
  <section class="process-group">
    <button class="process-heading" type="button" :aria-expanded="open" @click="manual = !open">
      <span class="caret">{{ open ? '⌄' : '›' }}</span>
      <span>{{ completed ? '已完成分析' : failed ? '分析已中断' : '分析过程' }}</span>
      <span v-if="tools.length" class="detail">{{ tools.length }} 次工具调用</span>
      <span v-if="issues" class="issues">{{ issues }} 项异常或受限</span>
    </button>
    <div v-if="open" class="process-content"><slot /></div>
  </section>
</template>

<style scoped>
.process-group {
  min-width: 0;
}
.process-heading {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  border: 0;
  background: transparent;
  color: var(--app-muted);
  padding: 10px 0;
  text-align: left;
  cursor: pointer;
  font: inherit;
  font-size: 12px;
}
.process-heading:hover {
  color: var(--app-text);
}
.caret {
  width: 12px;
  font-size: 18px;
}
.detail {
  color: var(--app-faint);
}
.issues {
  color: var(--app-warning, #d9a441);
}
.process-content {
  display: flex;
  flex-direction: column;
  gap: 8px;
  border-left: 1px solid var(--app-border);
  margin-left: 5px;
  padding: 0 0 8px 16px;
}
</style>
