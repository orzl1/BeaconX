# 术语表

产品文案、代码标识与文档必须使用同一套词。新增概念先加到这里，再落代码。

| 术语 | 英文 / 代码标识 | 含义 |
| --- | --- | --- |
| 任务 | Mission (`mission`) | 一次完整的授权测试活动，包含目标、范围、约束与产出 |
| 任务范围 | Mission Scope (`mission_scope`) | 机器可读的授权边界：域名 / IP / CIDR / URL + 允许的动作类别 |
| 资产 | Asset (`asset`) | 被测对象：根域、子域、IP、服务、应用、端点 |
| 资产身份 | Asset Identity (`asset_identity`) | 资产的归一化身份键（kind + key），用于去重与 upsert |
| 资产图 | Asset Graph | 跨任务共享的资产真值库 |
| 作战图 | Operation Graph (`operation_*`) | 单个任务的推进链：节点是知识，边是因果 |
| 假设 | Hypothesis (`hypothesis`) | 一个待验证的判断（取代旧式「意图」叙事） |
| 动作 | Action (`action`) | 最小工作单元：执行一次工具调用并写回结果 |
| 事实 | Fact (`fact`) | 已被观察证实的结论，必须可追溯到动作与证据 |
| 发现 | Finding (`finding`) | 具备安全影响的漏洞记录 |
| 证据 | Evidence (`evidence_snapshot`) | 从采集流量中冻结的不可变快照（哈希 + 版本） |
| 规划器 | Planner | 唯一生成假设/动作的角色，事件驱动唤醒 |
| 执行器 | Worker | 领取一个动作并执行，完成后写回图与事实 |
| 控制台 | Console | 人在环路的观察与操舵界面 |
| 策略检查 | Policy Check | 工具调用前的规则判定：allow / ask / deny |
| 审批 | Approval | 策略检查结果为 ask 时产生的人工决策项 |
| 审判模型 | Review Model | 可选的模型兜底判定（规则未命中时使用） |
| 审计事件 | Audit Event (`audit_event`) | 不可变的决策与调用记录 |
| 租户 | Tenant (`tenant`) | 数据与配置的逻辑归属单位；v1 单租户，字段与目录已预留 |
| 折叠 | Folding (`folding_members`) | 把大图中冷区节点折叠为摘要块，可逆展开 |
| 令牌账本 | Token Ledger (`token_ledger`) | 每次模型调用的用量事实表 |
