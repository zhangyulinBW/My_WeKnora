<template>
  <div class="admin-info-result">
    <div class="admin-info-header">
      <span class="admin-info-title">{{ $t('agentEditor.tools.recordAdminInfo') }}</span>
      <span class="admin-info-badge">{{ categoryLabel }}</span>
    </div>
    <div v-if="detailHtml" class="admin-info-detail" v-html="detailHtml"></div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { AdminInfoData } from '@/types/tool-results'
import {
  configureMarkedForChatMarkdown,
  createChatMarkdownRenderer,
  renderChatMarkdown,
} from '@/utils/chatMarkdownRenderer'
import { sanitizeMarkdownHTML, safeMarkdownToHTML } from '@/utils/security'

configureMarkedForChatMarkdown()
const markdownRenderer = createChatMarkdownRenderer({})

const props = defineProps<{
  data: AdminInfoData | Record<string, unknown>
}>()

const record = computed(() => (props.data || {}) as Partial<AdminInfoData>)

const CATEGORY_LABELS: Record<string, string> = {
  sql: 'SQL',
  fields: 'Fields',
  ids: 'ID',
  schema: 'Schema',
  notes: 'Notes',
}

const categoryLabel = computed(() => {
  const category = record.value.category || 'notes'
  return CATEGORY_LABELS[category] ?? category
})

const detailHtml = computed(() => {
  const detail = (record.value.detail || '').trim()
  if (!detail) return ''
  return renderChatMarkdown(detail, {
    renderer: markdownRenderer,
    escapeMarkdown: safeMarkdownToHTML,
    sanitizeHtml: sanitizeMarkdownHTML,
  })
})
</script>

<style scoped>
.admin-info-result {
  padding: 8px 0;
}

.admin-info-header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}

.admin-info-title {
  font-weight: 600;
  font-size: 13px;
}

.admin-info-badge {
  font-size: 11px;
  line-height: 1;
  padding: 3px 8px;
  border-radius: 999px;
  background: var(--td-brand-color-light, #e8f3ff);
  color: var(--td-brand-color, #0052d9);
}

.admin-info-detail {
  font-size: 13px;
  line-height: 1.6;
  word-break: break-word;
}

.admin-info-detail :deep(pre) {
  margin: 6px 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: var(--td-gray-color-1, #f2f2f2);
  overflow-x: auto;
}

.admin-info-detail :deep(code) {
  font-family: var(--td-font-family-monospace, monospace);
  font-size: 12px;
}

.admin-info-detail :deep(ul),
.admin-info-detail :deep(ol) {
  margin: 4px 0;
  padding-left: 18px;
}
</style>
