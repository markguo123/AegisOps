<script setup lang="ts">
defineProps<{ questions: { key: string; input: string }[]; active?: string }>()
defineEmits<{ navigate: [key: string] }>()
</script>
<template>
  <nav v-if="questions.length" class="minimap" aria-label="提问大纲">
    <el-tooltip v-for="q in questions" :key="q.key" placement="right" :show-after="0"
      :content="Array.from(q.input.trim()).slice(0, 15).join('')">
      <button type="button" :aria-label="q.input" :aria-current="active === q.key ? 'location' : undefined"
        @click="$emit('navigate', q.key)"><span /></button>
    </el-tooltip>
  </nav>
</template>
<style scoped>
.minimap { position: absolute; inset-inline-start: 2px; top: 50%; transform: translateY(-50%);
  z-index: 2; max-height: 80%; overflow: auto; scrollbar-width: none; }
button { display: flex; align-items: center; width: 16px; height: 12px; padding: 0; border: 0;
  background: transparent; color: var(--app-text); cursor: pointer; }
button span { width: 7px; height: 2px; background: currentColor; opacity: .3; }
button:hover span, button[aria-current] span { width: 14px; opacity: .95; }
</style>
