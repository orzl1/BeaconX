"use client";

import { useState } from "react";

import { addAssetIdentity, addCompanyScope, createAsset, createCompany, deleteAsset, deleteAssetIdentity, deleteCompany, deleteCompanyScope, listAssets, listCompanies, listCompanyScope, updateAsset, updateCompany } from "@/lib/api";
import { ASSET_TYPES, COMPANY_SCOPE_KINDS, label, parseObject } from "@/lib/format";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { Asset } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, LoadMore, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function AssetsPage({ token, onAuthError }: PageProps) {
  const [companyId, setCompanyId] = useState<number | null>(null);
  const [query, setQuery] = useState("");
  const [type, setType] = useState("");
  const companies = useApi({ token, interval: 10000, watch: [query], onAuthError, load: (current, signal) => listCompanies(current, { q: query || undefined, limit: 100 }, signal) });
  const scope = useApi({ token, enabled: Boolean(companyId), watch: [companyId], onAuthError, load: (current, signal) => listCompanyScope(current, companyId ?? 0, signal) });
  const assets = useApi({ token, interval: 10000, watch: [type], onAuthError, load: (current, signal) => listAssets(current, { type: type || undefined, limit: 100 }, signal) });
  const tail = useTail<Asset>(type);
  const busy = useBusy(onAuthError);
  const assetRows = [...(assets.data?.assets ?? []), ...tail.rows];
  const assetNext = tail.rows.length ? tail.next : (assets.data?.next_after_id ?? 0);
  const selected = (companies.data?.companies ?? []).find((company) => company.id === companyId);
  return (
    <>
      <PageHeader kicker="资产图" title="资产" description="带身份键创建时，命中已有资产会复用并更新，页面以返回的 ID 为准。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="企业">
          <div className="filters"><input value={query} placeholder="搜索名称或 slug" onChange={(event) => setQuery(event.target.value)} /></div>
          <form className="form-grid" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("co", async () => { const created = await createCompany(token, { name: String(data.get("name")), slug: String(data.get("slug")) }); companies.reload(); return `已创建 #${created.id}`; }); }}>
            <Field label="名称"><input name="name" required /></Field>
            <Field label="slug"><input name="slug" required /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <QueryBody token={token} loading={companies.loading} error={companies.error} empty={(companies.data?.companies ?? []).length === 0} emptyTitle="没有企业">
            {(companies.data?.companies ?? []).map((company) => (
              <div key={company.id} className="row-actions" style={{ marginBottom: 8 }}>
                <button type="button" className={company.id === companyId ? "btn btn-primary btn-sm" : "btn btn-ghost btn-sm"} onClick={() => setCompanyId(company.id)}>{company.name}</button>
                <span className="muted">{company.slug} · #{company.id}</span>
                <button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!window.confirm(`删除企业「${company.name}」？`)) return; void busy.run(`cd${company.id}`, async () => { await deleteCompany(token, company.id); if (companyId === company.id) setCompanyId(null); companies.reload(); }); }}>删除</button>
              </div>
            ))}
          </QueryBody>
          {companyId ? (
            <>
            <div style={{ marginTop: 12 }}>
              <p>范围</p>
              {(scope.data?.scope ?? []).map((item) => <p key={item.id}>{label(item.kind)} · {item.value} <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`cs${item.id}`, async () => { await deleteCompanyScope(token, companyId, item.id); scope.reload(); })}>移除</button></p>)}
              {scope.error ? <p className="muted">{scope.error}</p> : null}
              <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("scope", async () => { await addCompanyScope(token, companyId, { kind: String(data.get("kind")), value: String(data.get("value")), source: "manual" }); scope.reload(); }); }}>
                <select name="kind">{COMPANY_SCOPE_KINDS.map((kind) => <option key={kind} value={kind}>{label(kind)}</option>)}</select>
                <input name="value" required placeholder="值" />
                <button className="btn btn-primary btn-sm" type="submit">添加</button>
              </form>
            </div>
            {selected ? (
              <form className="form-grid" style={{ marginTop: 12 }} onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("co-edit", async () => { let attrs: Record<string, unknown> | null | undefined; const raw = String(data.get("attrs") || "").trim(); if (raw === "null") attrs = null; else if (raw) attrs = parseObject(raw); await updateCompany(token, selected.id, { name: String(data.get("name") || ""), slug: String(data.get("slug") || ""), ...(attrs !== undefined ? { attrs } : {}) }); companies.reload(); return "已更新企业"; }); }}>
                <Field label="改名称"><input name="name" defaultValue={selected.name} key={`n${selected.id}`} /></Field>
                <Field label="改 slug"><input name="slug" defaultValue={selected.slug} key={`s${selected.id}`} /></Field>
                <Field label="attrs" hint="JSON 对象整体替换；填 null 清空；留空不变"><textarea name="attrs" key={`a${selected.id}`} defaultValue={selected.attrs ? JSON.stringify(selected.attrs) : ""} /></Field>
                <button className="btn btn-primary btn-sm" type="submit">保存企业</button>
              </form>
            ) : null}
            </>
          ) : null}
        </Panel>
        <Panel title="资产">
          <div className="filters"><select value={type} onChange={(event) => setType(event.target.value)}><option value="">全部类型</option>{ASSET_TYPES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select></div>
          <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); const kind = String(data.get("kind") || ""); const key = String(data.get("key") || ""); void busy.run("asset", async () => { const parent = Number(data.get("parent_id")); const created = await createAsset(token, { type: String(data.get("type")), label: String(data.get("label")), parent_id: parent > 0 ? parent : undefined, identities: kind && key ? [{ kind, key }] : undefined }); assets.reload(); return `资产 #${created.id}。若身份已存在，这是被复用的 ID。`; }); }}>
            <div className="form-grid">
              <Field label="类型"><select name="type">{ASSET_TYPES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select></Field>
              <Field label="标签"><input name="label" required /></Field>
              <Field label="身份类型" hint="可空，填写则按身份 upsert"><select name="kind"><option value="">不带身份</option>{ASSET_TYPES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select></Field>
              <Field label="身份键"><input name="key" /></Field>
              <Field label="父资产 ID" hint="可空"><input name="parent_id" type="number" min={1} /></Field>
            </div>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <QueryBody token={token} loading={assets.loading} error={assets.error} empty={assetRows.length === 0} emptyTitle="没有资产">
            {assetRows.map((asset) => (
              <AssetRow key={asset.id} token={token} asset={asset} onChange={() => { assets.reload(); }} onAuthError={onAuthError} onDelete={() => { if (!window.confirm("删除该资产？子资产的父级会被清空。")) return; void busy.run(`ad${asset.id}`, async () => { await deleteAsset(token, asset.id); assets.reload(); }); }} />
            ))}
            <LoadMore nextId={assetNext} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more-assets", async () => { const page = await listAssets(token, { type: type || undefined, limit: 100, after_id: assetNext }); tail.push(page.assets ?? [], page.next_after_id); })} />
          </QueryBody>
        </Panel>
      </div>
    </>
  );
}

function AssetRow({ token, asset, onChange, onAuthError, onDelete }: { token: string; asset: Asset; onChange: () => void; onAuthError: (m: string) => void; onDelete: () => void }) {
  const busy = useBusy(onAuthError);
  const identities = asset.identities ?? [];
  return (
    <div style={{ borderTop: "1px solid var(--line)", padding: "8px 0" }}>
      <div className="row-actions"><strong>{asset.label} · {label(asset.type)} · #{asset.id}</strong><button type="button" className="btn btn-danger btn-sm" onClick={onDelete}>删除</button></div>
      {identities.map((item) => <p key={`${item.kind}:${item.key}`} className="muted">{label(item.kind)} · {item.key} <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("rm", async () => { await deleteAssetIdentity(token, asset.id, item.kind, item.key); onChange(); })}>移除</button></p>)}
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("id", async () => { await addAssetIdentity(token, asset.id, { kind: String(data.get("kind")), key: String(data.get("key")) }); onChange(); }); }}>
        <select name="kind">{ASSET_TYPES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select>
        <input name="key" required placeholder="身份键" />
        <button className="btn btn-ghost btn-sm" type="submit">加身份</button>
      </form>
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); const parentRaw = String(data.get("parent_id") ?? "").trim(); void busy.run("edit", async () => { await updateAsset(token, asset.id, { label: String(data.get("label") || asset.label), parent_id: parentRaw === "" ? undefined : parentRaw === "null" ? null : Number(parentRaw) }); onChange(); return "已更新资产"; }); }}>
        <input name="label" defaultValue={asset.label} aria-label="标签" />
        <input name="parent_id" placeholder={asset.parent_id ? `父级 ${asset.parent_id}，null 清空` : "父级 ID，null 清空"} aria-label="父资产" />
        <button className="btn btn-ghost btn-sm" type="submit">保存资产</button>
      </form>
      {busy.error ? <p className="muted">{busy.error}</p> : null}
    </div>
  );
}
