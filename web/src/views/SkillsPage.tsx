"use client";

import { useState } from "react";

import { createSkill, deleteSkill, deleteSkillFile, listSkillFiles, listSkills, listSkillVersions, readSkillFile, restoreSkillVersion, updateSkill, uploadFile, writeSkillFile } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function SkillsPage({ token, onAuthError }: PageProps) {
  const [name, setName] = useState("");
  const [path, setPath] = useState("");
  const [content, setContent] = useState("");
  const skills = useApi({ token, onAuthError, load: (current, signal) => listSkills(current, signal) });
  const files = useApi({ token, enabled: Boolean(name), watch: [name], onAuthError, load: (current, signal) => listSkillFiles(current, name, signal) });
  const versions = useApi({ token, enabled: Boolean(name), watch: [name], onAuthError, load: (current, signal) => listSkillVersions(current, name, signal) });
  const busy = useBusy(onAuthError);
  return (
    <>
      <PageHeader kicker="技能包" title="技能" description="上传 zip 不超过 10MB、200 个文件。版本默认保留最近若干份，恢复会回滚文件；旧快照没有元数据时只回滚文件。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="列表">
          <form className="form-grid" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { await createSkill(token, { name: String(data.get("name")), description: String(data.get("description") || ""), version: String(data.get("version") || "") }); skills.reload(); return "已创建"; }); }}>
            <Field label="名称"><input name="name" required pattern="[a-z0-9][a-z0-9_-]{0,63}" /></Field>
            <Field label="版本"><input name="version" placeholder="0.1.0" /></Field>
            <Field label="描述"><input name="description" /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <form onSubmit={(event) => { event.preventDefault(); const file = (event.currentTarget.elements.namedItem("file") as HTMLInputElement).files?.[0]; if (!file) return; void busy.run("zip", async () => { await uploadFile("/api/v1/skills/upload", token, file, "file"); skills.reload(); return "已上传"; }); }}>
            <Field label="上传 zip"><input name="file" type="file" accept=".zip,application/zip" /></Field>
            <button className="btn btn-ghost btn-sm" type="submit">上传</button>
          </form>
          <QueryBody token={token} loading={skills.loading} error={skills.error} empty={(skills.data?.skills ?? []).length === 0} emptyTitle="没有技能">
            {(skills.data?.skills ?? []).map((skill) => (
              <div key={skill.name} className="row-actions" style={{ marginTop: 8 }}>
                <button type="button" className={skill.name === name ? "btn btn-primary btn-sm" : "btn btn-ghost btn-sm"} onClick={() => setName(skill.name)}>{skill.name}</button>
                <span className="muted">{skill.version} · {skill.file_count} 文件</span>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => { const description = window.prompt("描述", skill.description); if (description === null) return; const version = window.prompt("版本", skill.version); if (version === null) return; void busy.run(`m${skill.name}`, async () => { await updateSkill(token, skill.name, { description, version }); skills.reload(); return "已更新技能"; }); }}>改元数据</button>
                <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`d${skill.name}`, async () => { await deleteSkill(token, skill.name); if (name === skill.name) setName(""); skills.reload(); })}>删除</button>
              </div>
            ))}
          </QueryBody>
        </Panel>
        <Panel title={name || "文件"}>
          {!name ? <p className="muted">先选择技能。</p> : (
            <>
              {(files.data?.files ?? []).map((file) => <p key={file.path}><button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("read", async () => { const data = await readSkillFile(token, name, file.path); setPath(data.path); setContent(data.content); })}>{file.path}</button> <span className="muted">{formatBytes(file.size)}</span> <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`df${file.path}`, async () => { await deleteSkillFile(token, name, file.path); files.reload(); skills.reload(); })}>删除</button></p>)}
              {files.error ? <p className="muted">{files.error}</p> : null}
              <Field label="路径"><input value={path} onChange={(event) => setPath(event.target.value)} /></Field>
              <Field label="内容"><textarea value={content} onChange={(event) => setContent(event.target.value)} /></Field>
              <button type="button" className="btn btn-primary btn-sm" disabled={!path} onClick={() => void busy.run("save", async () => { await writeSkillFile(token, name, path, content); files.reload(); skills.reload(); return "已保存"; })}>保存文件</button>
              <h3>版本</h3>
              {(versions.data?.versions ?? []).map((version) => (
                <p key={version.timestamp}>{version.timestamp} · {version.files} 文件 · {formatBytes(version.size)} {version.has_meta ? "" : "· 无元数据"} <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(version.timestamp, async () => { await restoreSkillVersion(token, name, version.timestamp); files.reload(); skills.reload(); versions.reload(); return "已恢复"; })}>恢复</button></p>
              ))}
              {(versions.data?.versions ?? []).length === 0 ? <p className="muted">还没有版本快照。</p> : null}
            </>
          )}
        </Panel>
      </div>
    </>
  );
}
