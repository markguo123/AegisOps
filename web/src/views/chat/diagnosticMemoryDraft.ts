export interface DiagnosticMemoryDraft {
  namespace: string
  relatedNamespaces: string[]
  workload: string
  text: string
}

const proposal = /(?:^|\r?\n)### 可保留的排查经验\r?\n主命名空间[：:]\s*([^\r\n]+)\r?\n关联命名空间[：:]\s*([^\r\n]+)\r?\n工作负载[：:]\s*([^\r\n]+)\r?\n经验[：:]\s*([^\r\n]+)\s*$/u
const label = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/u

export function parseDiagnosticMemoryDraft(text: string): DiagnosticMemoryDraft | undefined {
  const match = proposal.exec(text)
  if (!match) return
  const [, rawNamespace, rawRelated, rawWorkload, rawNote] = match
  const namespace = rawNamespace.trim()
  const workload = rawWorkload.trim()
  const note = rawNote.trim()
  const relatedNamespaces = rawRelated.trim() === '-'
    ? []
    : [...new Set(rawRelated.split(/[,，]/u).map(value => value.trim()))]
  if (!label.test(namespace) || relatedNamespaces.length > 10 ||
    !relatedNamespaces.every(value => label.test(value)) ||
    workload.length > 253 || !workload.split('.').every(value => label.test(value)) ||
    !note || [...note].length > 500) return
  return { namespace, relatedNamespaces, workload, text: note }
}
