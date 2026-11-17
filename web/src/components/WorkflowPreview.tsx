import { useRef, useState } from 'react';
import { ChevronDown, Play, Workflow } from 'lucide-react';
import { api } from '../api';
import type { WorkflowPreviewResult, WorkflowScenario } from '../api';
import { Pill } from '../components';

const scenarios: { id: WorkflowScenario; label: string }[] = [
  { id: 'answered', label: '有依据，完成回答' },
  { id: 'no_evidence', label: '无依据，直接拒答' },
  { id: 'simulated_timeout', label: '模拟模型超时' },
];

const nodes: Record<string, string> = {
  retrieve: '检索依据',
  compose: '生成并校验',
  no_evidence: '结束：证据不足',
  retrieval_failed: '结束：检索失败',
};
const outcomes: Record<string, string> = {
  OK: '完成，引用已校验',
  NO_EVIDENCE: '无依据，未调用模型',
  MODEL_TIMEOUT: '模拟模型超时，流程已结束',
};

export default function WorkflowPreview({ publicDemo = false }: { publicDemo?: boolean } = {}) {
  const [scenario, setScenario] = useState<WorkflowScenario>('answered');
  const [result, setResult] = useState<WorkflowPreviewResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const inFlight = useRef(false);

  async function run() {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError('');
    setResult(null);
    try { setResult(await api<WorkflowPreviewResult>('workflow-preview', { scenario })); }
    catch (failure) { setError(publicDemo ? '编排预览暂不可用，请稍后重试。' : (failure as Error).message); }
    finally { inFlight.current = false; setBusy(false); }
  }

  return (
    <details className="workflow-preview">
      <summary>
        <Workflow size={18} aria-hidden="true" />
        <span>LangGraph 编排预览</span>
        <Pill tone="cyan">{publicDemo ? '公开样例 · 模拟模型' : '本机模拟'}</Pill>
        <ChevronDown size={16} className="workflow-chevron" aria-hidden="true" />
      </summary>
      <div className="workflow-body">
        <p>用固定案例执行真实 LangGraph。只模拟模型，不读取上方输入，不连接 Qwen 或集群。</p>
        <div className="workflow-controls">
          <label htmlFor="workflow-scenario">演示场景</label>
          <select id="workflow-scenario" value={scenario} disabled={busy} onChange={event => {
            setScenario(event.target.value as WorkflowScenario);
            setResult(null);
            setError('');
          }}>
            {scenarios.map(option => <option key={option.id} value={option.id}>{option.label}</option>)}
          </select>
          <button type="button" className="button secondary" onClick={() => void run()} disabled={busy}>
            <Play size={14} aria-hidden="true" />{busy ? '正在运行…' : '运行编排预览'}
          </button>
        </div>
        {busy && <p role="status">正在运行固定案例，等待本次节点记录。</p>}
        {error && <p className="workflow-error" role="alert">{error}</p>}
        {result && <section className="workflow-result" aria-label="本次编排结果" aria-live="polite">
          <ol className="workflow-track" aria-label="实际执行的节点">
            {result.run.trace.map((node, index) => <li key={`${index}-${node}`}>
              <span className="workflow-step-index" aria-hidden="true">{String(index + 1).padStart(2, '0')}</span>
              <span><strong>{nodes[node] ?? node}</strong><code>{node}</code></span>
            </li>)}
          </ol>
          <div className="workflow-outcome">
            <strong>{outcomes[result.run.error_code] ?? '流程已结束'}</strong>
            <code>{result.run.error_code}</code>
          </div>
          <p>模拟模型调用 {result.run.synthetic_model_candidate_counts.length} 次，引用 {result.run.citation_count} 条。
            LangGraph {result.langgraph}，语料快照 {result.evaluation_as_of.slice(0, 10)}。</p>
        </section>}
      </div>
    </details>
  );
}
