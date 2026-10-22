"use client";

import { useState } from "react";

import { deleteWorkspace, listMissions, listWorkspace, readWorkspace, uploadFile, writeWorkspace } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function WorkspacePage({ token, onAuthError }: PageProps) {
  const [missionId, setMissionId] = useState("");
  const [path, setPath] = useState("");
  const [content, setContent] = useState("");
  const missions = useApi({ token, onAuthError, load: (current, signal) => listMissions(current, { limit: 100 }, signal) });
  const id = Number(missionId);
  const files = useApi({ token, enabled: id > 0, watch: [missionId], onAuthError, load: (current, signal) => listWorkspace(current, id, "", signal) });
  const busy = useBusy(onAuthError);
  const quota = files.data;
  return (
    <>
      <PageHeader kicker="任务工作目录" title="工作区" description="必须先选定任务。文本读写上限内由后端校验，超配额会返回 413。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="filters">
        <select value={missionId} onChange={(event) => setMissionId(event.target.value)} aria-label="任务">
          <option value="">选择任务</option>
          {(missions.data?.missions ?? []).map((mission) => <option key={mission.id} value={mission.id}>#{mission.id} {mission.name}</option>)}
        </select>
        {quota ? <span className="muted">{formatBytes(quota.used_bytes)} / {formatBytes(quota.quota_bytes)} · {quota.root}</span> : null}
      </div>
      {!id ? <p className="muted">先选择任务。</p> : (
        <div className="grid-2">
          <Panel title="文件">
            <QueryBody token={token} loading={files.loading} error={files.error} empty={(files.data?.files ?? []).length === 0} emptyTitle="目录是空的">
              {(files.data?.files ?? []).map((file) => (
                <div key={file.path} className="row-actions" style={{ marginBottom: 6 }}>
                  <button type="button" className="btn btn-ghost btn-sm" disabled={file.is_dir} onClick={() => void busy.run("read", async () => { const data = await readWorkspace(token, id, file.path); setPath(data.path); setContent(data.content); })}>{file.path}</button>
                  <span className="muted">{file.is_dir ? "目录" : formatBytes(file.size)}</span>
                  <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`del${file.path}`, async () => { await deleteWorkspace(token, id, file.path, file.is_dir); files.reload(); })}>删除</button>
                </div>
              ))}
            </QueryBody>
            <form onSubmit={(event) => { event.preventDefault(); const file = (event.currentTarget.elements.namedItem("file") as HTMLInputElement).files?.[0]; if (!file) return; void busy.run("up", async () => { await uploadFile(`/api/v1/workspace/upload${`?mission_id=${id}`}`, token, file, "file"); files.reload(); return "已上传"; }); }}>
              <Field label="上传"><input name="file" type="file" /></Field>
              <button className="btn btn-ghost btn-sm" type="submit">上传</button>
            </form>
          </Panel>
          <Panel title="编辑">
            <Field label="路径"><input value={path} onChange={(event) => setPath(event.target.value)} placeholder="notes.txt" /></Field>
            <Field label="内容"><textarea value={content} onChange={(event) => setContent(event.target.value)} /></Field>
            <button type="button" className="btn btn-primary btn-sm" disabled={!path} onClick={() => void busy.run("save", async () => { const saved = await writeWorkspace(token, id, path, content); files.reload(); return `已写入 ${saved.path}（${formatBytes(saved.size)}）`; })}>保存</button>
          </Panel>
        </div>
      )}
    </>
  );
}
