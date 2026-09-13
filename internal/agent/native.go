package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

var ErrToolProtocolFailures = errors.New("连续 3 次原生工具调用失败，请检查模型和接口工具适配")

// Native declarations and the execution gate share the same role contract.
func (a *Agent) toolDefinitions() []llm.ToolDefinition {
	if a.toolsDisabled {
		return nil
	}
	definitions := append(tools.Definitions(), forum.Definitions()...)
	definitions = append(definitions, specialToolDefinitions()...)
	allowed := definitions[:0]
	for _, definition := range definitions {
		if a.toolPermission(definition.Name) == "" {
			allowed = append(allowed, definition)
		}
	}
	return allowed
}

func (a *Agent) toolDefinitionTokens() int {
	definitions := a.toolDefinitions()
	if len(definitions) == 0 {
		return 0
	}
	data, _ := json.Marshal(definitions)
	return estimateTextTokens(string(data)) + 8
}

func (a *Agent) toolPermission(name string) string {
	if a.toolsDisabled {
		return "工具调用已禁用；请根据已有证据直接给出结论。"
	}
	if name == "worker_irc_reply" && (a.phase == phaseModerator || a.verifying || a.ircTool == nil) {
		return "只有收到已投递 IRC 的原始成员可回复。"
	}
	if a.ircOnly {
		switch name {
		case "worker_irc_reply", "read_file", "list_files", "search_content", "search_context", "read_tool_buffer", "read_handoff", "review_state", "forum_read", "forum_threads", "forum_roster", "load_skill":
		default:
			return "已完成成员仅被唤醒回答 IRC；不能重写阶段状态或重复投票，后续工作由管理员明确重开。"
		}
	}
	if strings.HasPrefix(name, "moderator_") || name == "forum_moderate" || name == "forum_announce" {
		if a.phase != phaseModerator {
			return "此工具仅允许论坛管理员调用。"
		}
	}
	if a.verifying {
		switch name {
		case "read_file", "list_files", "search_content", "search_context", "read_tool_buffer", "read_handoff", "review_state", "load_skill", "forum_post", "forum_read", "forum_threads", "forum_wait", "forum_roster":
		default:
			return "验证 Agent 只能读取源码与状态、加载技能和参与论坛，不能更改审计状态、提交漏洞或结束阶段。"
		}
	}
	if name == "load_skill" && len(a.prompts.Skills) == 0 {
		return "没有可加载的技能。"
	}
	if forum.IsTool(name) && a.board == nil {
		return "论坛不可用。"
	}
	return a.phaseToolCorrection(name)
}

func specialToolDefinitions() []llm.ToolDefinition {
	makeTool := func(name, description, schema string) llm.ToolDefinition {
		return llm.ToolDefinition{Type: "function", Name: name, Description: description, Parameters: json.RawMessage(schema)}
	}
	return []llm.ToolDefinition{
		makeTool("read_handoff", "读取前一阶段完整结构化交接；超长结果按工具 buffer 分页。", `{"type":"object","properties":{},"additionalProperties":false}`),
		makeTool("load_skill", "按名称加载本次任务需要的可用技能。", `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		makeTool("audit_plan_done", "提交侦察交接并结束当前侦察 worker。audit_files 必须是实际文件路径。", `{"type":"object","properties":{"summary":{"type":"string"},"audit_map":{"type":"string"},"audit_files":{"type":"array","items":{"type":"string"}},"execute_instructions":{"type":"string"}},"required":["summary","audit_map","audit_files"],"additionalProperties":false}`),
		makeTool("end_audit", "请求本阶段团队关闭并明确投票；失败后继续有价值的审计。", `{"type":"object","properties":{"summary":{"type":"string"},"next_steps":{"type":"string"},"vote":{"type":"string","enum":["approve","reject"]}},"required":["summary","vote"],"additionalProperties":false}`),
		makeTool("verify_finding", "启动独立验证 Agent 复核候选漏洞，不直接登记漏洞。", `{"type":"object","properties":{"severity":{"type":"string"},"title":{"type":"string"},"path":{"type":"string"},"line":{"type":"integer"},"evidence":{"type":"string"},"impact":{"type":"string"},"recommendation":{"type":"string"},"cwe":{"type":"string"}},"required":["title","path","evidence"],"additionalProperties":false}`),
		makeTool("moderator_idle", "结束本次管理员激活，等待新活动。", `{"type":"object","properties":{},"additionalProperties":false}`),
		makeTool("moderator_review_state", "读取成员与漏洞快照、稳定 finding_key。", `{"type":"object","properties":{},"additionalProperties":false}`),
		makeTool("moderator_revoke_finding", "依据源码反证撤销漏洞，保留原记录。", `{"type":"object","properties":{"finding_key":{"type":"string"},"reason":{"type":"string"},"evidence":{"type":"string"}},"required":["finding_key","reason","evidence"],"additionalProperties":false}`),
		makeTool("moderator_assign", "审计阶段建议后续复核；侦察阶段仅记录待审计建议随交接传递，不重开侦察。", `{"type":"object","properties":{"agent_id":{"type":"string"},"content":{"type":"string"}},"required":["agent_id","content"],"additionalProperties":false}`),
		makeTool("moderator_irc_send", "中断当前阶段成员的模型请求或工具等待，安全边界优先投递。queued/delivered 不代表已回答。", `{"type":"object","properties":{"agent_id":{"type":"string"},"content":{"type":"string","maxLength":4096}},"required":["agent_id","content"],"additionalProperties":false}`),
		makeTool("moderator_irc_read", "读取持久 IRC 状态及相关回复、目标实时不可变进度；after_id 是变更游标，支持最多120秒等待；长结果按 buffer 分页。", `{"type":"object","properties":{"message_id":{"type":"integer"},"after_id":{"type":"integer"},"limit":{"type":"integer","minimum":1,"maximum":64},"timeout_seconds":{"type":"integer","minimum":0,"maximum":120}},"additionalProperties":false}`),
		makeTool("worker_irc_reply", "明确回复已经投递给自己的 IRC 消息 ID；公开保存并通知管理员。不替代阶段完成或投票。", `{"type":"object","properties":{"message_id":{"type":"integer"},"content":{"type":"string","maxLength":4096}},"required":["message_id","content"],"additionalProperties":false}`),
		makeTool("moderator_decide", "仅对 audit 阶段结束审查作出批准或继续工作决定；侦察不需要批准。", `{"type":"object","properties":{"stage":{"type":"string","enum":["audit"]},"decision":{"type":"string","enum":["approve","request_work"]},"reason":{"type":"string"},"assignments":{"type":"array","items":{"type":"object","properties":{"agent_id":{"type":"string"},"content":{"type":"string"}},"required":["agent_id","content"],"additionalProperties":false}}},"required":["stage","decision","reason"],"additionalProperties":false}`),
	}
}

func validateNativeResponse(response llm.ToolResponse, history []llm.Message) error {
	if len(response.Calls) > 1 {
		return fmt.Errorf("模型返回 %d 个原生工具调用；每回合只允许一个，整批未执行", len(response.Calls))
	}
	for _, call := range response.Calls {
		arguments := strings.TrimSpace(call.Arguments)
		if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" || !json.Valid([]byte(arguments)) || !strings.HasPrefix(arguments, "{") {
			return fmt.Errorf("原生工具调用缺少 call_id/name 或完整 JSON 对象参数；未执行")
		}
		for _, message := range history {
			if message.Type == "function_call" && message.CallID == call.CallID {
				return fmt.Errorf("原生工具调用重复 call_id %q；未执行", call.CallID)
			}
		}
	}
	return nil
}

func (a *Agent) recordResponse(response llm.ToolResponse) (llm.FunctionCall, bool) {
	if text := joinAssistantMessage(response.Thinking, response.Content); text != "" {
		a.addMessage(llm.Message{Role: llm.RoleAssistant, Content: text})
	}
	if len(response.Calls) == 0 {
		return llm.FunctionCall{}, false
	}
	call := response.Calls[0]
	a.addMessage(llm.Message{Type: "function_call", ID: call.ID, CallID: call.CallID, Name: call.Name, Arguments: call.Arguments})
	return call, true
}

func (a *Agent) recordToolOutput(call llm.FunctionCall, result, full string) {
	message := llm.Message{Type: "function_call_output", CallID: call.CallID, Content: result}
	a.messages = append(a.messages, message)
	message.Content = full
	a.appendTraceMessage(message)
}

func (a *Agent) rejectTool(call llm.FunctionCall, reason string) {
	data, _ := json.Marshal(tools.Result{OK: false, Error: reason})
	full := string(data)
	a.recordToolOutput(call, a.tools.BoundResult(call.Name, full), full)
}

func (a *Agent) executeNativeTool(ctx context.Context, emit func(Event), call llm.FunctionCall) string {
	if err := ctx.Err(); err != nil {
		a.rejectTool(call, "调用执行前已取消；未执行："+err.Error())
		return ""
	}
	if call.Name != "read_tool_buffer" {
		a.tools.ClearBuffer()
	}
	allowed := false
	for _, definition := range a.toolDefinitions() {
		if definition.Name == call.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		reason := a.toolPermission(call.Name)
		if reason == "" {
			reason = "不存在或当前角色不可用的工具：" + call.Name + "。请从当前原生工具定义中选择。"
		}
		a.rejectTool(call, reason)
		return ""
	}
	if call.Name == "end_audit" {
		if blocker := a.endAuditNeedsConfirmation(); blocker != "" {
			a.rejectTool(call, blocker)
			return ""
		}
	} else {
		a.pendingEndAudit = false
	}
	emit(Event{Kind: "tool", Content: "calling " + call.Name})
	result, full := a.callTool(ctx, emit, ToolCall{Name: call.Name, Arguments: json.RawMessage(call.Arguments)})
	a.recordToolOutput(call, result, full)
	return full
}

func validateNativeHistory(messages []llm.Message) error {
	seen := make(map[string]bool)
	pending := ""
	for _, message := range messages {
		switch message.Type {
		case "function_call":
			if pending != "" || seen[message.CallID] {
				return fmt.Errorf("native history contains duplicate or unpaired function call")
			}
			if err := validateNativeResponse(llm.ToolResponse{Calls: []llm.FunctionCall{{CallID: message.CallID, Name: message.Name, Arguments: message.Arguments}}}, nil); err != nil {
				return err
			}
			pending = message.CallID
			seen[pending] = true
		case "function_call_output":
			if pending == "" || message.CallID != pending {
				return fmt.Errorf("native history contains orphan function output")
			}
			pending = ""
		default:
			if pending != "" {
				return fmt.Errorf("native history interrupts a pending function call")
			}
		}
	}
	if pending != "" {
		return fmt.Errorf("native history ends with an unpaired function call")
	}
	return nil
}
