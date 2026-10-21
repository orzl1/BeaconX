// 极简展示组件：不引入任何 UI 库，样式集中在 src/styles.css。
// 这些组件都是纯函数，不持有状态，因此不需要 "use client"。

import type { ReactNode } from "react";

type Tone = "neutral" | "info" | "success" | "warning" | "danger" | "accent";

export function Badge({ children, tone = "neutral" }: { children: ReactNode; tone?: Tone | string }) {
  return <span className={`badge badge-${tone}`}>{children}</span>;
}

export function SectionCard({
  title,
  description,
  actions,
  children,
  headingId,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  headingId?: string;
}) {
  return (
    <section className="card" aria-labelledby={headingId}>
      <header className="card-head">
        <div>
          <h2 className="card-title" id={headingId}>
            {title}
          </h2>
          {description ? <p className="card-sub">{description}</p> : null}
        </div>
        {actions ? <div className="card-actions">{actions}</div> : null}
      </header>
      <div className="card-body">{children}</div>
    </section>
  );
}

export function EmptyState({ title, hint }: { title: string; hint?: ReactNode }) {
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      {hint ? <p className="empty-hint">{hint}</p> : null}
    </div>
  );
}

export function ErrorBanner({
  message,
  onDismiss,
  id,
}: {
  message: string;
  onDismiss?: () => void;
  id?: string;
}) {
  return (
    <div className="banner banner-error" role="alert" id={id}>
      <span className="banner-text">{message}</span>
      {onDismiss ? (
        <button type="button" className="btn btn-ghost btn-sm" onClick={onDismiss} aria-label="关闭错误提示">
          关闭
        </button>
      ) : null}
    </div>
  );
}

export function NoticeBanner({ message, id }: { message: string; id?: string }) {
  return (
    <div className="banner banner-info" role="status" id={id}>
      <span className="banner-text">{message}</span>
    </div>
  );
}

/** JSON 展示：参数、事件负载、更新响应等都复用这一处，保证格式一致。 */
export function JsonBlock({ value, label }: { value: unknown; label?: string }) {
  let text: string;
  if (value === undefined) {
    text = "（无数据）";
  } else if (value === null) {
    text = "null";
  } else if (typeof value === "string") {
    text = value;
  } else {
    try {
      text = JSON.stringify(value, null, 2);
    } catch {
      text = String(value);
    }
  }
  return (
    <div className="json-wrap">
      {label ? <span className="json-label">{label}</span> : null}
      <pre className="json">{text}</pre>
    </div>
  );
}

/** 简单的横向进度条；未设置上限时调用方应展示"不限"而不是进度条。 */
export function ProgressBar({ percent, label }: { percent: number; label?: string }) {
  const value = Math.max(0, Math.min(100, percent));
  return (
    <div className="progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(value)}>
      <div className="progress-fill" style={{ width: `${value}%` }} />
      {label ? <span className="progress-label">{label}</span> : null}
    </div>
  );
}
