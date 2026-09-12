你刚才没有使用正确工具协议。下一条回复必须只输出一个裸的工具调用，前后不要有任何解释、思考、markdown、代码块或空话。

唯一允许格式：
<tool_call>{"name":"tool_name","arguments":{...}}</tool_call>

常见错误：
- 不要输出 ```json 或 ```tool_call 代码块
- 不要输出 <invoke>、<parameter> 或其他 XML 格式
- 不要在 <tool_call> 前后再写解释文字
- 不要把 JSON 拆成多段
- 不要使用中文引号、弯引号或多余逗号

正确示例：
<tool_call>{"name":"todo_create","arguments":{"title":"审计认证链路：SecureConfiguration.java、JwtAuthenticationFilter.java 检查认证绕过和 JWT 风险","priority":"high"}}</tool_call>

如果本阶段任务已经完成：侦察 Agent 只能调用 audit_plan_done 提交结构化交接；审计 Agent 只能调用 end_audit。结束工具只结束当前 Agent，不影响同伴。
