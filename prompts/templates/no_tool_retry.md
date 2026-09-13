你刚才的请求已完成，但没有返回原生工具调用。若仍有工作，请从本次 API 提供的工具定义中选择一个函数，通过原生 function calling 提交完整 JSON 对象参数；普通文本、推理中的示例、XML/DSML 和代码块都不会执行。不要把没有调用误认为网络停滞。

每回合最多一个调用。侦察完成后调用 audit_plan_done 提交结构化交接。!{audit_completion_policy}
