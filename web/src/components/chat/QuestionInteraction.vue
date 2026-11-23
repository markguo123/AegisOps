<script setup lang="ts">
import { ref } from 'vue'
import type { QuestionUiData } from './turn'

/**
 * 结构化提问卡（Interaction kind=question）：
 * - 选项来自模型真实调用，选择只表达业务决策，不授予写权限；
 * - 自由输入与跳过遵循既有校验；Secret 值永远不会走这条通道。
 */
const props = defineProps<{ question: QuestionUiData }>()
const emit = defineEmits<{
  (e: 'answer', payload: { interactionId: string; status: 'selected' | 'text' | 'skipped'; optionId?: string; text?: string }): void
}>()

const selected = ref('')
const custom = ref(false)
const text = ref('')
const busy = ref(false)
const pending = ref(props.question.status === 'pending')

function submit(skip = false): void {
  if (!pending.value || busy.value) {
    return
  }
  busy.value = true
  emit('answer', {
    interactionId: props.question.interactionId,
    status: skip ? 'skipped' : custom.value ? 'text' : 'selected',
    optionId: custom.value || skip ? undefined : selected.value,
    text: custom.value ? text.value : undefined,
  })
}
</script>

<template>
  <section class="question-interaction" aria-label="待回答的问题">
    <header class="question-head">
      <span class="question-label">需要你的选择</span>
      <span v-if="!pending" class="question-state">{{ question.status }}</span>
    </header>
    <strong class="question-prompt">{{ question.prompt }}</strong>
    <label v-for="option in question.options" :key="option.id" class="question-option">
      <input v-model="selected" type="radio" :value="option.id" :disabled="busy || !pending" @change="custom = false" />
      <span>
        {{ option.label }}{{ option.recommended ? '（推荐）' : '' }}
        <small v-if="option.impact">{{ option.impact }}</small>
      </span>
    </label>
    <label v-if="pending" class="question-custom">
      <input v-model="custom" type="checkbox" :disabled="busy" /> 自由输入
    </label>
    <textarea
      v-if="pending && custom"
      v-model="text"
      maxlength="2000"
      :disabled="busy"
      placeholder="描述你的选择；不要填写密码或 Secret"
    />
    <p v-if="question.answer" class="question-answer">已答复：{{ question.answer }}</p>
    <div v-if="pending" class="question-actions">
      <button type="button" :disabled="busy" @click="submit(true)">跳过</button>
      <button type="button" :disabled="busy || (custom ? !text.trim() : !selected)" @click="submit()">
        {{ busy ? '提交中…' : '提交选择' }}
      </button>
    </div>
  </section>
</template>

<style scoped>
.question-interaction {
  padding: 14px 16px;
  border: 1px solid var(--app-accent-border);
  border-radius: var(--app-radius);
  background: var(--app-surface);
}

.question-head {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 6px;
}

.question-label {
  color: var(--app-accent-2);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.04em;
  text-transform: uppercase;
}

.question-state {
  color: var(--app-muted);
  font-size: 11px;
}

.question-prompt {
  display: block;
  margin-bottom: 8px;
  font-size: 13px;
  line-height: 1.6;
}

.question-option {
  display: flex;
  gap: 10px;
  margin: 8px 0;
  cursor: pointer;
  font-size: 13px;
}

.question-option small {
  display: block;
  margin-top: 3px;
  color: var(--app-muted);
  font-size: 12px;
}

.question-custom {
  display: inline-flex;
  gap: 6px;
  align-items: center;
  color: var(--app-muted);
  font-size: 12px;
}

textarea {
  display: block;
  width: 100%;
  min-height: 64px;
  margin: 8px 0;
  padding: 8px;
  border: 1px solid var(--app-border);
  border-radius: 8px;
  background: var(--app-canvas);
  color: var(--app-text);
  font: inherit;
}

.question-answer {
  margin: 8px 0 0;
  color: var(--app-muted);
  font-size: 12px;
}

.question-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 12px;
}

.question-actions button {
  padding: 5px 14px;
  border: 1px solid var(--app-border);
  border-radius: 999px;
  background: transparent;
  color: var(--app-text);
  cursor: pointer;
}

.question-actions button:last-child {
  border-color: var(--app-accent);
  background: var(--app-accent-soft);
  color: var(--app-accent-2);
}
</style>
