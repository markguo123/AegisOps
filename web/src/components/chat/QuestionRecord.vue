<script setup lang="ts">
import { computed } from 'vue'
import type { UserQuestion } from '@/api/sessions'
const props = defineProps<{ value: unknown }>()
const record = computed(() => props.value as { question?: UserQuestion; answer?: { status: string; option_id?: string; text?: string } } | null)
const answer = computed(() => {
  const a = record.value?.answer
  if (!a) return ''
  if (a.status === 'text') return a.text
  if (a.status === 'selected') return record.value?.question?.options.find(o => o.id === a.option_id)?.label
  return ({ skipped: '已跳过', unavailable: '当前模式无法提问', timed_out: '等待超时', canceled: '已取消', invalid: '回答无效' } as Record<string, string>)[a.status] ?? a.status
})
</script>
<template>
  <div v-if="record?.question">
    <p>{{ record.question.question }}</p>
    <ul><li v-for="option in record.question.options" :key="option.id">{{ option.label }}：{{ option.impact }}</li></ul>
    <p><strong>用户决策：</strong>{{ answer }}</p>
  </div>
</template>
