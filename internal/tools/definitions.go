package tools

import (
	"encoding/json"

	"code-review-agent/internal/llm"
)

// Definitions returns independently owned native schemas. Agent-only operations,
// including end_audit, are defined and permission-filtered by the agent runtime.
func Definitions() []llm.ToolDefinition {
	searchParameters := `{"type":"object","properties":{
		"query":{"type":"string","description":"搜索内容；优先使用此字段。默认普通字符串，正则必须指定 mode=regex。"},
		"keyword":{"type":"string","description":"query 为空时的备用内容，优先于 text。"},
		"text":{"type":"string","description":"query/keyword 为空时的备用内容。"},
		"content":{"type":"string","description":"query/keyword/text 为空时的备用内容。"},
		"needle":{"type":"string","description":"上述内容为空时的备用内容，优先于 term。"},
		"term":{"type":"string","description":"备用搜索内容。"},
		"pattern":{"type":"string","description":"兼容参数：文件 glob 用于 include；无搜索内容且不是文件 glob 时作为正则 query。优先明确使用 query、mode、include。"},
		"mode":{"type":"string","description":"使用 literal、regex 或 fuzzy；默认 literal。兼容 text/contains/substring/exact、regexp/regular_expression/regular-expression、fuzz/keyword/keywords；未知值退回 literal。"},
		"root":{"type":"string","description":"工作区相对搜索根目录，省略为工作区。"},
		"path":{"type":"string","description":"root 为空时的备用根路径，优先于 dir。"},
		"dir":{"type":"string","description":"root/path 为空时的备用根目录。"},
		"directory":{"type":"string","description":"root/path/dir 为空时的备用根目录。"},
		"include":{"type":"string","description":"文件 glob 或逗号/分号分隔的模式，支持 **，相对搜索根且不区分大小写。"},
		"includes":{"type":"array","items":{"type":"string"},"description":"附加文件 glob 列表。"},
		"file_pattern":{"type":"string","description":"include 为空时的备用文件模式。"},
		"file_patterns":{"type":"array","items":{"type":"string"},"description":"includes 为空时的备用文件模式列表。"},
		"limit":{"type":"integer","description":"最多命中数；省略或非正数为 100。"},
		"case_insensitive":{"type":"boolean","description":"默认 true；case_sensitive=true 时本参数被覆盖。"},
		"case_sensitive":{"type":"boolean","description":"true 强制区分大小写，默认 false。"}
	},"anyOf":[{"required":["query"]},{"required":["keyword"]},{"required":["text"]},{"required":["content"]},{"required":["needle"]},{"required":["term"]},{"required":["pattern"]}]}`
	return []llm.ToolDefinition{
		definition("list_files", "列出工作区内文件和目录；启动时已预载清单，通常仅在需要刷新视图时调用。", `{"type":"object","properties":{
			"root":{"type":"string","description":"工作区相对根路径；省略为整个工作区。"},
			"pattern":{"type":"string","description":"按文件或目录基本名称匹配的 glob，不区分大小写。"},
			"max_depth":{"type":"integer","description":"相对根路径的最大深度；非正数不限。"},
			"include_hidden":{"type":"boolean","description":"是否包含以点开头的隐藏文件/目录；默认 false。"},
			"limit":{"type":"integer","description":"最多返回条目数；省略或非正数为 500。"}
		}}`),
		definition("read_file", "按行读取文件，返回真实相对路径、总行数及带行号内容。优先沿用已有工具返回的相对路径，不要自行猜测绝对路径。", `{"type":"object","properties":{
			"path":{"type":"string","minLength":1,"description":"文件路径，优先工作区相对路径。"},
			"offset":{"type":"integer","description":"起始行号，从 1 开始；省略或非正数为 1。"},
			"limit":{"type":"integer","description":"读取行数；省略或非正数为 120。"}
		},"required":["path"]}`),
		definition("read_tool_buffer", "分页读取本 Agent 上一次超长工具结果。仅连续调用本工具保留 buffer；任何其他工具会先清空它，不落盘。用 next_offset 续读。", `{"type":"object","properties":{
			"buffer_id":{"type":"string","minLength":1,"description":"上一次结果返回的当前 buffer 标识，不得编造。"},
			"offset":{"type":"integer","minimum":0,"description":"UTF-8 字节偏移，默认 0；必须在正文内且是字符边界。"},
			"limit":{"type":"integer","minimum":0,"description":"请求内容字节数；0 或超过工具结果预算时使用预算，最小实际值为 4；JSON 开销可能进一步缩短正文。"}
		},"required":["buffer_id"]}`),
		definition("search_content", "搜索文件内容；支持跨行正则及模糊搜索，默认不区分大小写。ok=true 且 data=null 表示执行成功但无匹配。", searchParameters),
		definition("search_context", "搜索文件内容，与 search_content 相同；优先调用 search_content。", searchParameters),
		definition("git_inspect", "只读 Git 检查；仅 Git 可用且工作区是仓库时调用。支持增量审计、提交历史和 blame，不执行任何写入命令。", `{"type":"object","properties":{
			"action":{"type":"string","enum":["status","changed_files","diff","log","show","blame"],"description":"检查动作，默认 status；blame 必须提供 path。"},
			"base":{"type":"string","description":"diff/changed_files 比较起点；与 head 形成 base..head。引用不得以 - 开头或包含空白。"},
			"head":{"type":"string","description":"比较终点，默认 HEAD。"},
			"ref":{"type":"string","description":"show 的引用，默认 commit 或 HEAD；允许 Git 引用字符，不接受选项或空白。"},
			"commit":{"type":"string","description":"ref 为空时 show 使用的引用。"},
			"path":{"type":"string","description":"工作区内相对路径过滤；blame 必填。show 指定时读取该版本文件，否则返回提交概要。"},
			"line_start":{"type":"integer","description":"blame 起始行，正数时启用行范围。"},
			"line_end":{"type":"integer","description":"blame 结束行；小于 line_start 时改为 line_start。"},
			"limit":{"type":"integer","description":"log 最多提交数；省略或非正数为 50。"},
			"context":{"type":"integer","description":"diff 上下文行数；默认 0，负数按 0。"},
			"staged":{"type":"boolean","description":"diff/changed_files 仅暂存改动；优先于 unstaged 和 base。"},
			"unstaged":{"type":"boolean","description":"仅未暂存改动。changed_files 中优先于 base；diff 中 base 优先于此参数。"}
		},"allOf":[{"if":{"properties":{"action":{"const":"blame"}},"required":["action"]},"then":{"required":["path"],"properties":{"path":{"minLength":1}}}}]}`),
		definition("todo_create", "创建具体中文审计待办；标题应包含文件、目录、模块、入口函数、变量或明确审计点，避免空泛描述。", `{"type":"object","properties":{
			"title":{"type":"string","minLength":1,"description":"具体中文待办标题。"},
			"priority":{"type":"string","description":"优先级，建议 low/medium/high；省略或空值为 medium。"}
		},"required":["title"]}`),
		definition("todo_update", "更新待办。优先用 ID 定位；无正 ID 时按唯一完整标题匹配。完成时使用 completed。", `{"type":"object","properties":{
			"id":{"type":["integer","string"],"description":"既有待办 ID，支持整数或整数字符串；正 ID 优先于标题匹配。"},
			"title":{"type":"string","description":"有正 ID 时替换标题；否则用此完整标题定位唯一待办。"},
			"status":{"type":"string","description":"新状态，建议 pending/in_progress/completed；done 会转换为 completed；空值保留原状态。"},
			"priority":{"type":"string","description":"新优先级，建议 low/medium/high；空值保留原值。"}
		},"anyOf":[{"required":["id"]},{"required":["title"]}]}`),
		definition("file_review_update", "将文件加入本次审计范围并更新状态。items 优先于 paths、path、inventory 选择器；目录/后缀/模式选择器取并集，无匹配可成功返回空结果。不得未读内容就批量标记 reviewed。", `{"type":"object","properties":{
			"path":{"type":"string","description":"单个文件相对路径。"},
			"paths":{"type":"array","items":{"type":"string"},"description":"批量相对路径。"},
			"dir":{"type":"string","description":"inventory 目录选择器，包含子目录。"},
			"dirs":{"type":"array","items":{"type":"string"},"description":"多个目录选择器。"},
			"suffix":{"type":"string","description":"文件后缀选择器，例如 .go。"},
			"suffixes":{"type":"array","items":{"type":"string"},"description":"多个后缀选择器。"},
			"pattern":{"type":"string","description":"inventory 相对路径 glob 选择器，区分大小写；* 不跨越路径分隔符，单独 * 匹配全部。"},
			"patterns":{"type":"array","items":{"type":"string"},"description":"多个 glob 选择器。"},
			"status":{"type":"string","description":"建议 unseen/reviewing/reviewed/skipped；空值默认 reviewed，也用作 items 缺省状态。"},
			"note":{"type":"string","description":"中文用途、发现或结论；空值不覆盖已有笔记，也用作 items 缺省笔记。"},
			"items":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string","minLength":1},"status":{"type":"string","description":"空值继承顶层 status，仍为空则为 reviewed。"},"note":{"type":"string","description":"空值继承顶层 note。"}},"required":["path"]},"description":"逐文件独立状态/笔记。"}
		},"anyOf":[{"required":["path"]},{"required":["paths"]},{"required":["items"]},{"required":["dir"]},{"required":["dirs"]},{"required":["suffix"]},{"required":["suffixes"]},{"required":["pattern"]},{"required":["patterns"]}]}`),
		definition("variable_review_update", "记录变量、函数或符号的来源、传播路径和风险；用 name 与 path 联合定位。报告漏洞前先记录关键变量。", `{"type":"object","properties":{
			"name":{"type":"string","minLength":1,"description":"变量、函数或符号名称。"},
			"path":{"type":"string","description":"所属相对路径，与 name 组成定位键。"},
			"status":{"type":"string","description":"建议 tracking/reviewed/suspicious/benign；空值默认 tracking。"},
			"note":{"type":"string","description":"中文来源、传播、风险或结论；空值不覆盖已有笔记。"}
		},"required":["name"]}`),
		definition("flow_review_update", "记录跨文件调用链、数据流和后续排查；flow 是临时队列，闭环后移除，不长期累积。", `{"type":"object","properties":{
			"name":{"type":"string","minLength":1,"description":"唯一 flow 名称。"},
			"status":{"type":"string","description":"默认 tracking；tracking/suspicious/confirmed 保留队列，reviewed/benign/done/closed 会关闭并移除。"},
			"entry":{"type":"string","description":"入口函数或入口路径。"},
			"files":{"type":"array","items":{"type":"string"},"description":"链路涉及的文件；空列表不覆盖已有值。"},
			"variables":{"type":"array","items":{"type":"string"},"description":"关键变量或符号；空列表不覆盖已有值。"},
			"evidence":{"type":"string","description":"源码证据、传播链和 sink。"},
			"next_step":{"type":"string","description":"下一步具体排查动作。"},
			"note":{"type":"string","description":"中文排查记录；可选文本空值不覆盖已有值。"}
		},"required":["name"]}`),
		definition("flow_review_delete", "删除已闭环、已转为漏洞或不再需要展示的临时 flow；名称不存在会报错。", `{"type":"object","properties":{"name":{"type":"string","minLength":1,"description":"既有 flow 名称。"}},"required":["name"]}`),
		definition("review_state", "查看待办、项目笔记、文件/变量/flow 排查及漏洞状态；结束前检查实际覆盖。", `{"type":"object","properties":{"limit":{"type":"integer","description":"文件、变量、flow 三类各自最多条目数；省略或非正数为 80；不截断待办和漏洞。"}}}`),
		definition("project_note_update", "替换完整项目级中文工作笔记，详细维护架构、认证鉴权、攻击面、状态流、文件角色、证据与待确认问题，而非仅写摘要。", `{"type":"object","properties":{"note":{"type":"string","minLength":1,"description":"完整笔记正文；替换旧笔记，不是追加。"}},"required":["note"]}`),
		definition("report_finding", "提交高置信度漏洞；团队按 FIFO 排队串行去重审核和写入。提交者等待结果，重复则返回 existing_key 而不新增；审核不确定或失败不登记。先补全证据并参考 verify_finding。", `{"type":"object","properties":{
			"severity":{"type":"string","description":"按系统严重性分级选择 critical/high/medium/low；省略或空值默认为 medium。"},
			"title":{"type":"string","minLength":1,"description":"中文漏洞标题。"},
			"path":{"type":"string","minLength":1,"description":"主要证据所在文件路径。"},
			"line":{"type":"integer","description":"主要证据行号，省略为 0。"},
			"evidence":{"type":"string","minLength":1,"description":"可核对的源码证据、触发条件和完整利用链。"},
			"impact":{"type":"string","description":"可证明的实际安全影响。"},
			"recommendation":{"type":"string","description":"针对根因的中文修复建议。"},
			"cwe":{"type":"string","description":"CWE 标识，可省略。"}
		},"required":["title","path","evidence"]}`),
	}
}

func definition(name, description, parameters string) llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Name: name, Description: description, Parameters: json.RawMessage(parameters)}
}
