import { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Activity, ArrowUpRight, ChartNoAxesCombined, Search, ShieldCheck, Workflow, X } from 'lucide-react';
import { api } from '../api';
import type { Citation, QueryResult } from '../api';
import { Heading, Pill } from '../components';
import WorkflowPreview from '../components/WorkflowPreview';
import '../styles.css';

const demoQuestion = 'Pod UID 变化后旧审批还能用吗？';
const questions = [demoQuestion, 'Claim 与 Approval 有什么区别？', 'Grafana 访问失败需要重装吗？'];
const sectionOf = (c: Citation) => c.section_path.split('/').at(-1) || c.section_path;
const titleOf = (c: Citation) => c.section_path.split('/')[0];
const pageRoute = () => location.hash === '#evaluation' ? 'evaluation' : 'rag';

type Source = { id: string; title: string; version: number; content_sha256: string;
  source_locator: string; source_kind: 'public_demo_sample'; text: string };
type Comparison = { question: string; variants: {
  label: string; context_characters: number; citations: Citation[];
}[] };
type OpenSource = (citation: Citation, snippet?: string) => void;

function ContestApp() {
  const [route, setRoute] = useState(pageRoute);
  const [source, setSource] = useState<{ doc: Source; snippet?: string } | null>(null);
  const [sourceError, setSourceError] = useState('');
  useEffect(() => {
    function onRoute() { setRoute(pageRoute()); }
    window.addEventListener('hashchange', onRoute);
    return () => window.removeEventListener('hashchange', onRoute);
  }, []);
  const openSource: OpenSource = async (citation, snippet) => {
    setSourceError('');
    try {
      const doc = await api<Source>(`documents/${encodeURIComponent(citation.document_id)}`);
      setSource({ doc, snippet });
    } catch { setSourceError('暂时无法查看原文，请重试。'); }
  };
  return <div className="app">
    <aside className="sidebar">
      <a href="#rag" className="brand"><span className="brand-symbol"><Activity size={25} /></span>
        <span><strong>Kubernetes SRE 智能运维助手</strong><small>证据驱动的运维工作台</small></span></a>
      <div className="workspace-label"><span className="status-light" />未来杯 · 赛题一</div>
      <nav aria-label="主导航">
        <a href="#rag" className={route === 'rag' ? 'selected' : ''} aria-current={route === 'rag' ? 'page' : undefined}><Workflow />RAG 工作台</a>
        <a href="#evaluation" className={route === 'evaluation' ? 'selected' : ''} aria-current={route === 'evaluation' ? 'page' : undefined}><ChartNoAxesCombined />评测中心</a>
      </nav>
      <div className="sidebar-bottom"><div className="local-guard"><ShieldCheck /><div><strong>只读演示</strong>
        <p>不连接私网模型或真实集群</p></div></div></div>
    </aside>
    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb">未来杯<span>/</span><strong>比赛演示</strong></div>
        <Pill tone="cyan">公开教学样例</Pill></header>
      <main id="main-content">
        {route === 'rag' ? <SearchPage openSource={openSource} /> : <ComparisonPage openSource={openSource} />}
      </main>
      <footer className="app-footer"><span>知识参考不等于现场证据。</span><span>不会执行运维动作</span></footer>
    </div>
    {sourceError && <div className="toast" role="alert">{sourceError}</div>}
    {source && <SourceDialog {...source} close={() => setSource(null)} />}
  </div>;
}

function SearchPage({ openSource }: { openSource: OpenSource }) {
  const [query, setQuery] = useState('');
  const [algorithm, setAlgorithm] = useState('bm25');
  const [result, setResult] = useState<QueryResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function search() {
    if (busy || !query.trim()) return;
    setBusy(true); setError(''); setResult(null);
    try { setResult(await api<QueryResult>('retrieve', { query: query.trim(), algorithm })); }
    catch { setError('检索未完成，请检查输入后重试。'); }
    finally { setBusy(false); }
  }
  return <>
    <Heading title="RAG 工作台" description="输入一个运维问题，查看命中的证据和原文。" aside={<Pill tone="cyan">实际检索</Pill>} />
    <div className="rag-layout"><div>
      <section className="query-panel"><form onSubmit={e => { e.preventDefault(); void search(); }}>
        <label htmlFor="contest-question" className="query-label"><Search size={19} />你想了解什么？</label>
        <textarea id="contest-question" value={query} onChange={e => setQuery(e.target.value)}
          placeholder={demoQuestion} maxLength={500} autoComplete="off" />
        <div className="query-toolbar"><div className="query-options">
          <label>算法<select value={algorithm} disabled={busy} onChange={e => setAlgorithm(e.target.value)}>
            <option value="bm25">BM25</option><option value="keyword">Keyword</option></select></label>
        </div><button className="button primary" disabled={busy || !query.trim()}><Search size={16} />
          {busy ? '正在检索…' : '检索知识'}</button></div>
        <div className="query-hint"><span>只检索公开样例，请勿输入敏感信息。</span><span>{query.length}/500</span></div>
      </form></section>
      <section className="query-starters"><span>示例问题</span>
        {questions.map(q => <button key={q} onClick={() => setQuery(q)}>{q}<ArrowUpRight size={14} /></button>)}</section>
      {error && <p className="inline-error" role="alert">{error}</p>}
      {result && <section className="results" aria-live="polite">
        <div className="results-heading"><h2>检索结果 <span>{result.context.items.length}</span></h2>
          <Pill>{result.algorithm.toUpperCase()} · {result.elapsed_ms} ms</Pill></div>
        {!result.context.items.length && <p className="empty-state">公开样例中没有匹配片段。试试上面的示例题。</p>}
        {result.context.items.map(({ statement, citations: [citation] }, index) =>
          <article className="evidence-card" key={citation.chunk_id}>
            <div className="evidence-card-top"><span className="rank-number">{String(index + 1).padStart(2, '0')}</span>
              <div><h3>{titleOf(citation)}</h3><span className="section-path">{sectionOf(citation)}</span></div>
              <Pill>v{citation.document_version}</Pill></div>
            <pre className="snippet expanded">{statement}</pre>
            <div className="evidence-card-footer"><button className="text-button accent"
              onClick={() => openSource(citation, statement)}>定位原文<ArrowUpRight size={15} /></button></div>
          </article>)}
      </section>}
      <WorkflowPreview publicDemo />
    </div><aside className="rag-aside"><section className="panel">
      <h2>试用路线</h2><p className="muted-copy">从示例题开始，打开命中原文；到评测中心对比两种依据，再运行编排预览。</p>
      <a href="#evaluation" className="text-link">查看同题对照<ArrowUpRight size={15} /></a>
    </section></aside></div>
  </>;
}

function ComparisonPage({ openSource }: { openSource: OpenSource }) {
  const [data, setData] = useState<Comparison | null>(null);
  const [error, setError] = useState(false);
  useEffect(() => {
    let active = true;
    api<Comparison>('evidence-comparison').then(value => { if (active) setData(value); })
      .catch(() => { if (active) setError(true); });
    return () => { active = false; };
  }, []);
  return <>
    <Heading title="评测中心" description="同题对照：完整五段 vs 双定义" aside={<Pill>公开样例</Pill>} />
    <p className="notice-line">本页只比较证据片段和正文字符数，不调用模型，也不展示私网实验的答案或耗时。</p>
    {error ? <p role="alert">对照暂不可用，请稍后刷新。</p> : !data ? <p role="status">正在读取对照……</p> :
      <section className="evaluation-detail">
        <div className="question-heading"><Pill tone="cyan">同一道题</Pill><h2>{data.question}</h2></div>
        <div className="rank-columns">{data.variants.map(variant => <article className="panel rank-panel" key={variant.label}>
          <div className="panel-heading"><h3>{variant.label}</h3><span>{variant.citations.length} 段 · {variant.context_characters} 字符</span></div>
          <ol>{variant.citations.map((c, i) => <li key={c.chunk_id}><span>{i + 1}</span>
            <button onClick={() => openSource(c)}>{sectionOf(c)}<ArrowUpRight size={14} /></button>
          </li>)}</ol>
        </article>)}</div>
      </section>}
  </>;
}

function SourceDialog({ doc, snippet, close }: { doc: Source; snippet?: string; close: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { const element = dialog.current; element?.showModal(); return () => element?.close(); }, []);
  const match = snippet ? doc.text.indexOf(snippet) : -1;
  return <dialog className="source-dialog" ref={dialog} onCancel={close} aria-labelledby="contest-source-heading">
    <header><div><Pill tone="cyan">公开教学样例</Pill><h2 id="contest-source-heading">{doc.title}</h2></div>
      <button className="icon-button" aria-label="关闭原文" onClick={close}><X size={20} /></button></header>
    <div className="source-meta"><span>版本 v{doc.version}</span></div>
    <dl className="source-identity"><dt>来源</dt><dd>{doc.source_locator}</dd><dt>SHA-256</dt><dd>{doc.content_sha256}</dd></dl>
    <div className="source-scroll"><pre className="source-body">{match >= 0 && snippet
      ? <>{doc.text.slice(0, match)}<mark>{snippet}</mark>{doc.text.slice(match + snippet.length)}</> : doc.text}</pre></div>
    <p className="source-note">只读原文。按 Esc 关闭。</p>
  </dialog>;
}

createRoot(document.getElementById('root')!).render(<ContestApp />);
