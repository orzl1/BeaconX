package plan

// ReadySteps 计算当前可以派发的步骤 key，顺序与 steps 的声明顺序一致。
//
// 判定规则（两条同时满足）：
//
//  1. 步骤自身状态严格等于 StepPending——ready / running / done / skipped /
//     failed，以及 status 里缺失的 key，都不算「待派发」；
//  2. 所有依赖的状态严格等于 StepDone。依赖不在 status 中视为未完成
//     （fail closed）；依赖不在本次 steps 里、但 status 明确是 done 的（例如
//     前一次决策声明的步骤）同样算满足。
//
// 这里有意不做任何状态迁移或副作用：把 pending 改成 ready、发事件、起执行器
// 都是调度层的职责。领域层只回答「现在哪些步骤可以开始」。
func ReadySteps(steps []Step, status map[string]StepStatus) []string {
	var ready []string
	for _, s := range steps {
		if status[s.Key] != StepPending {
			continue
		}
		satisfied := true
		for _, dep := range s.DependsOn {
			if status[dep] != StepDone {
				satisfied = false
				break
			}
		}
		if satisfied {
			ready = append(ready, s.Key)
		}
	}
	return ready
}
