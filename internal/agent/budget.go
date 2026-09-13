package agent

import (
	"context"
	"fmt"
	"math"
	"time"

	"code-review-agent/internal/llm"
)

// BudgetStatus is task-scoped. Pausing stops the clock, not accumulated usage.
// Elapsed is saved as a duration, never as a process-local clock/deadline.
type BudgetStatus struct {
	InfiniteMode bool          `json:"infinite_mode"`
	Hours        int           `json:"hours"`
	Minutes      int           `json:"minutes"`
	TokenLimit   int64         `json:"token_limit"`
	UsedTokens   int64         `json:"used_tokens"`
	Elapsed      time.Duration `json:"elapsed"`
	Running      bool          `json:"running"`
	StopReason   string        `json:"stop_reason,omitempty"`
	Estimated    bool          `json:"estimated"`
}

type auditBudget struct {
	elapsed           time.Duration
	started           time.Time
	tokens            int64
	estimated         bool
	estimatedRequests int
	requests          map[uint64]llm.UsageUpdate
	reason            string
	timer             *time.Timer
}

func (t *Team) BudgetStatus() BudgetStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.budgetStatusLocked()
}

func (t *Team) budgetStatusLocked() BudgetStatus {
	elapsed := t.budget.elapsed
	if !t.budget.started.IsZero() {
		elapsed += time.Since(t.budget.started)
	}
	return BudgetStatus{InfiniteMode: t.cfg.Agent.InfiniteMode, Hours: t.cfg.Agent.BudgetHours, Minutes: t.cfg.Agent.BudgetMinutes, TokenLimit: t.cfg.Agent.BudgetTokens, UsedTokens: t.budget.tokens, Elapsed: elapsed, Running: t.running, StopReason: t.budget.reason, Estimated: t.budget.estimated || t.budget.estimatedRequests > 0}
}

// ConfigureBudget changes limits only. A depleted budget cannot be bypassed by
// pause/go, or by confirming the same limits again.
func (t *Team) ConfigureBudget(infinite bool, hours, minutes int, tokens int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return fmt.Errorf("请先暂停并等待团队停止，再修改预算")
	}
	cfg := t.cfg.Agent
	cfg.InfiniteMode, cfg.BudgetHours, cfg.BudgetMinutes, cfg.BudgetTokens = infinite, hours, minutes, tokens
	if _, err := cfg.BudgetDuration(); err != nil {
		return err
	}
	t.cfg.Agent = cfg
	for _, w := range t.workers {
		w.agent.cfg.Agent = cfg
	}
	if t.moderator != nil {
		t.moderator.agent.cfg.Agent = cfg
	}
	t.budget.reason = t.budgetLimitLocked()
	return nil
}

func (t *Team) budgetLimitLocked() string {
	if !t.cfg.Agent.InfiniteMode {
		return ""
	}
	duration, _ := t.cfg.Agent.BudgetDuration()
	if duration > 0 && t.budgetStatusLocked().Elapsed >= duration {
		return "时间预算已耗尽"
	}
	if t.cfg.Agent.BudgetTokens > 0 && t.budget.tokens >= t.cfg.Agent.BudgetTokens {
		return "token 预算已耗尽"
	}
	return ""
}

// Called under t.mu after admission, before any model or tool starts.
func (t *Team) startBudgetLocked(ctx context.Context, cancel context.CancelFunc) context.Context {
	t.budget.reason = t.budgetLimitLocked()
	if t.budget.reason != "" {
		cancel()
		return ctx
	}
	t.budget.started = time.Now()
	t.budget.requests = make(map[uint64]llm.UsageUpdate)
	t.budget.estimatedRequests = 0
	duration, _ := t.cfg.Agent.BudgetDuration()
	if t.cfg.Agent.InfiniteMode && duration > 0 {
		t.budget.timer = time.AfterFunc(duration-t.budget.elapsed, func() {
			t.mu.Lock()
			if ctx.Err() == nil && t.running && t.budget.reason == "" {
				t.budget.reason = "时间预算已耗尽"
				cancel()
			}
			t.mu.Unlock()
		})
	}
	return llm.WithUsageObserver(ctx, func(update llm.UsageUpdate) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if update.TotalTokens < 0 {
			return
		}
		previous := t.budget.requests[update.RequestID]
		delta := update.TotalTokens - previous.TotalTokens
		if delta > 0 && t.budget.tokens > math.MaxInt64-delta {
			t.budget.tokens = math.MaxInt64
		} else {
			t.budget.tokens += delta
		}
		if previous.Estimated {
			t.budget.estimatedRequests--
		}
		if update.Final {
			delete(t.budget.requests, update.RequestID)
			t.budget.estimated = t.budget.estimated || update.Estimated
		} else {
			t.budget.requests[update.RequestID] = update
			if update.Estimated {
				t.budget.estimatedRequests++
			}
		}
		if reason := t.budgetLimitLocked(); reason != "" && t.budget.reason == "" {
			t.budget.reason = reason
			cancel()
		}
	})
}

// All actors and moderator have drained before this runs.
func (t *Team) finishBudget() {
	t.mu.Lock()
	if t.budget.timer != nil {
		t.budget.timer.Stop()
		t.budget.timer = nil
	}
	if !t.budget.started.IsZero() {
		t.budget.elapsed += time.Since(t.budget.started)
		t.budget.started = time.Time{}
	}
	t.budget.estimated = t.budget.estimated || t.budget.estimatedRequests > 0
	t.budget.estimatedRequests = 0
	t.budget.requests = nil
	reason := t.budget.reason
	if reason != "" {
		for _, w := range t.workers {
			if !w.saved.Completed {
				w.saved.Status.Status = "cancelled"
				w.saved.Status.Activity = reason + "，调整预算后可继续"
			}
		}
	}
	t.mu.Unlock()
	if reason != "" {
		t.publish(Event{Kind: "info", Content: reason + "；已停止整个团队，保留审计进度（不是审计完成）"})
		t.emitState()
	}
}

func (a *Agent) auditCompletionPolicy() string {
	if a.cfg.Agent.InfiniteMode {
		return "无限审计模式：系统不提供结束审计或投票关闭工具。持续检查覆盖空白、复核证据、协作交叉审计并维护具体待办；完成当前计划后自行寻找有价值的后续工作，不以普通回答宣布结束，不反复刷空操作。只有用户停止或团队时间/token任一预算耗尽才停止，不能自行声称预算耗尽。侦察仍通过 audit_plan_done 交接到审计，不结束整个任务。"
	}
	return "审计结束前先调用 review_state 核对待办、入口、文件、变量和跨文件 flow 的实际覆盖。没有覆盖的安全相关入口继续建立具体待办；不能因已发现漏洞、文件很多或暂时无法闭环就收工。end_audit 是团队关闭请求：summary 说明完整覆盖与剩余项无继续价值的证据，next_steps 可选，vote 必须明确 approve 或 reject。全部审计成员在本轮截止前明确 approve 才真正结束；沉默不算同意。pending、reject 或超时后继续自己的审计：自行选择尚未覆盖的其他文件、入口或模块，建立具体待办、读取源码、追踪新的调用链。不要等待、催票、反复请求关闭，也不要围绕其他 Agent 已有结论重复复核；没有新的独立审计进展，不重复提交同一关闭请求。"
}
