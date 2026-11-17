export interface Doc {
  id: string; title: string; version: number; status: string; review_due_at: string;
  expired: boolean; source_locator: string; content_sha256: string;
  sources: { path: string; sha256: string }[]; evaluation_only: boolean; characters: number; text?: string;
}
export interface Citation { document_id: string; document_version: number; section_path: string; chunk_id: string; source_locator: string }
export type SavedCitation = Omit<Citation, 'source_locator'>;
export interface SavedQuestion {
  question_id: string; question: string; answer: string; status: string; error_code: string;
  citations: SavedCitation[]; duration_seconds: number | null; response_sha256: string;
  original_ranks?: number[];
  definition_coverage?: { cited: number; required: number };
  measurement: { request_sha256: string; duration_unit: 'ns'; ollama_metrics: Record<string, number | null>;
    sent_sources: { source_id: string; citations: SavedCitation[] }[] } | null;
  review: { verdict: string; review_kind: string; structure_check: string; citation_check: string;
    semantic_review: string; required_document_coverage: string; missing_required_document_ids: string[];
    checks: { assertion: string; verdict: string; reason: string; evidence: string; kind: string }[] } | null;
}
export interface SavedAnswerEvaluations {
  batches: { id: string; label: string; model: string; evaluation_as_of: string; algorithm: string;
    ollama_version: string; deferred_count: number | null; questions: SavedQuestion[];
    model_digest: string; system_prompt_sha256: string; corpus_manifest_sha256: string | null;
    question_files_sha256: Record<string, string> | null }[];
}
export interface KnowledgeItem { statement: string; scope: string; uncertainty: string; citations: Citation[] }
export interface QueryResult { context: { query_id: string; generated_at: string; items: KnowledgeItem[] }; algorithm: string; elapsed_ms: number; timing_scope: string; model: string }
export interface AnswerResult {
  answer: { schema_version: string; query_id: string; scope: string;
    status: 'ANSWERED' | 'INSUFFICIENT_EVIDENCE'; answer: string; citations: Citation[]; error_code: string };
  model: string; elapsed_ms: number; timing_scope: string;
}
export type WorkflowScenario = 'answered' | 'no_evidence' | 'simulated_timeout';
export interface WorkflowPreviewResult {
  langgraph: string; evaluation_as_of: string; model: 'synthetic_only';
  run: {
    case: WorkflowScenario; trace: string[]; status: string; error_code: string;
    citation_count: number; synthetic_model_candidate_counts: number[];
  };
}
export interface Row { question_id: string; retrieved_document_ids: string[]; document_recall_at_5: number; reciprocal_rank: number }
export interface Report {
  algorithm: string; evaluation_as_of: string; generated_at: string; question_count: number;
  scored_retrieval_questions: number; deferred_question_ids: string[]; retrieval_results: Row[];
  negative_cases: { case_id: string; passed: boolean }[];
  metrics: { document_recall_at_5: number; mean_reciprocal_rank: number; citation_text_and_metadata_valid: number; negative_case_pass_rate: number };
}
export interface Overview { documents: Doc[]; reports: { keyword: Report; bm25: Report }; questions: Record<string, string>; mode: string; model: string; cluster: string; capabilities: Record<string, string> }

const messages: Record<string, string> = {
  VERIFIED_DATA_UNAVAILABLE: '资料完整性验证失败。请核对本地清单与来源文件，不要绕过哈希校验。',
  INVALID_INPUT: '输入无效，请检查问题长度和算法选项。', NOT_FOUND: '未找到该登记文档。',
  MODEL_NOT_CONNECTED: '真实模型未接入。本次只能检索已批准的离线资料。',
  MODEL_CALL_NOT_APPROVED: '请先确认允许将本次问题和命中片段发送给私网模型。',
  MODEL_BUSY: '私网模型已有问答正在处理，请等待完成后再提交。',
  MODEL_TIMEOUT: '私网模型等待超时。未返回草稿，请先确认模型已空闲再重试。',
  MODEL_UNAVAILABLE: '无法连接私网模型，请确认三台虚拟机和 Ollama 服务已启动。',
  MODEL_HTTP_ERROR: '私网模型拒绝了本次请求；未展示回答，请保留错误情况排查。',
  MODEL_OUTPUT_INVALID: '私网模型返回的格式或引用未通过校验，回答已拒绝展示。',
  MODEL_RESPONSE_INVALID: '私网模型未返回完整且匹配的响应，回答已拒绝展示。',
  MODEL_RESPONSE_TOO_LARGE: '私网模型响应过大，已停止读取。',
  CONTEXT_BUDGET_EXCEEDED: '本次知识片段超过上下文预算，请缩小问题范围；未调用模型。',
  ORIGIN_DENIED: '访问来源未获允许，请使用本机工作台地址。',
  GRAPH_PREVIEW_UNAVAILABLE: '本机编排预览环境不可用。普通检索和问答不受影响，请核对独立 LangGraph 环境。',
  GRAPH_PREVIEW_DEADLINE: '本机预览运行超过等待时间，已结束本次运行。可以重试。',
};
export async function api<T>(path: string, body?: unknown): Promise<T> {
  let response: Response;
  try { response = await fetch(`/api/v1/${path}`, { method: body === undefined ? 'GET' : 'POST', headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store' }); }
  catch { throw new Error('无法连接本地只读接口。请启动 127.0.0.1:8765 后重试。'); }
  const data = await response.json().catch(() => null);
  if (!response.ok || data?.code !== 'OK') throw new Error(messages[data?.code] || '本地接口暂不可用，请稍后重试。');
  return data.data as T;
}
