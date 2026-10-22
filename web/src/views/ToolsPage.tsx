"use client";

import { Link, paths } from "@/lib/router";
import { PageHeader, Panel } from "@/components/ui";

const BUILTINS = [
  {
    name: "http_probe",
    risk: "中",
    role: "worker",
    summary: "向目标发送一次 HTTP 请求，返回状态码、响应头与正文片段（最多 32KB）。",
    fields: [
      ["url", "必填", "完整 URL，只支持 http/https"],
      ["method", "可选", "HTTP 方法，默认 GET"],
      ["headers", "可选", "附加请求头"],
      ["body", "可选", "请求体"],
      ["timeout_seconds", "可选", "超时秒数，1–60"],
    ],
  },
  {
    name: "shell_exec",
    risk: "高",
    role: "worker",
    summary: "执行一条 shell 命令并返回合并后的输出（最多 32KB）。通常需要人工审批。",
    fields: [
      ["command", "必填", "要执行的命令"],
      ["timeout_seconds", "可选", "超时秒数，1–120"],
      ["work_dir", "可选", "工作目录"],
    ],
  },
];

export function ToolsPage() {
  return (
    <>
      <PageHeader
        kicker="执行器登记"
        title="工具"
        description="没有工具清单接口。下面两行来自后端内置注册，不是实时查询。调用必须经过执行器、策略门和审批。自定义工具与参数保存还没有 API。"
        actions={<Link href={paths.policy} className="btn btn-primary btn-sm">去策略试算</Link>}
      />
      {BUILTINS.map((tool) => (
        <Panel key={tool.name} title={tool.name} extra={<span className="muted">风险 {tool.risk} · 角色 {tool.role}</span>}>
          <p>{tool.summary}</p>
          <table>
            <thead><tr><th>参数</th><th>是否必填</th><th>说明</th></tr></thead>
            <tbody>
              {tool.fields.map((field) => <tr key={field[0]}><td className="num">{field[0]}</td><td>{field[1]}</td><td>{field[2]}</td></tr>)}
            </tbody>
          </table>
        </Panel>
      ))}
    </>
  );
}
