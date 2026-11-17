import { useState } from 'react';
import { LockKeyhole, Search, GitBranch, FileCheck2 } from 'lucide-react';
import type { ReactNode } from 'react';
import type { Overview } from './api';

export const dateOnly = (value: string) => new Intl.DateTimeFormat('sv-SE', { timeZone: 'UTC', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(value));
export function Pill({ children, tone = '' }: { children: ReactNode; tone?: string }) { return <span className={`pill ${tone}`}>{children}</span>; }
export function Heading({ title, description, aside }: { title: string; description: string; aside?: ReactNode }) {
  return <header className="page-heading"><div><h1>{title}</h1><p>{description}</p></div>{aside}</header>;
}
export function titleOf(data: Overview, id: string) { return data.documents.find(d => d.id === id)?.title || id; }
const steps = [
  { title: '提出问题', icon: Search, detail: '输入一次具体问题。本地工作台不保存问题历史，也不启用多轮记忆。' },
  { title: '准入过滤', icon: LockKeyhole, detail: '服务端确定身份，再检查 Tenant、ACL、审核状态和复审期限。不接受浏览器自报权限。' },
  { title: '词法检索', icon: GitBranch, detail: '在可用片段中使用 keyword 或 BM25 排序。它不是语义模型，也不产生执行授权。' },
  { title: '绑定引用', icon: FileCheck2, detail: '返回原文及文档版本、章节和片段哈希。按需生成时，模型只能选择本次来源编号，由程序绑定回真实引用。' },
];
export function EvidenceRail({ compact = false }: { compact?: boolean }) {
  const [active, setActive] = useState(2);
  return <div className={`evidence-rail ${compact ? 'compact' : ''}`}>
    <div className="rail-track" role="group" aria-label="证据路径阶段">{steps.map((s, i) => <div className="rail-piece" key={s.title}>
      <button className={`rail-step ${active === i ? 'active' : ''}`} onClick={() => setActive(i)} aria-pressed={active === i}>
        <span className="rail-index" aria-hidden="true">0{i + 1}</span><s.icon aria-hidden="true" /><span>{s.title}</span>
      </button>
    </div>)}</div>
    <div className="rail-detail"><small>流程说明，非实时 Trace</small><span>{steps[active].detail}</span></div>
  </div>;
}
