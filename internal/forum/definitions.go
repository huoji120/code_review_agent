package forum

import (
	"encoding/json"

	"code-review-agent/internal/llm"
)

// Definitions returns independently owned schemas. Board.Call remains the
// authorization boundary, even when callers filter administrator-only tools.
func Definitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		definition("forum_post", "在公开论坛发帖或回复，不是私信。嵌套回复归原线程；关闭线程禁止回复但保留历史。正文提及 @全体成员、@全体 或 @all 可通知其他成员；仅指定 to 或阅读不订阅线程。", `{"type":"object","properties":{
			"to":{"type":"string","description":"已注册 Agent ID 或 *；省略为空。所有交流仍属公开论坛。"},
			"reply_to":{"type":"integer","minimum":0,"description":"保留消息 ID；0 或省略为新帖，正数回复该消息所属线程。"},
			"topic":{"type":"string","description":"标题，不超过 512 UTF-8 字节；可省略。"},
			"content":{"type":"string","minLength":1,"description":"正文，去除空白后非空，且原文不超过 16384 UTF-8 字节。证据必须写完整，不使用占位内容。"}
		},"required":["content"]}`),
		definition("forum_threads", "按页发现论坛帖子，置顶优先，其余按最近活动排序。返回有界 excerpt 与来源消息 ID，不是完整正文；完整内容用 forum_read 正文分页。", `{"type":"object","properties":{
			"page":{"type":"integer","minimum":0,"description":"页号，从 1 开始；0 或省略为 1，超末页时取末页。"},
			"limit":{"type":"integer","minimum":0,"description":"每页帖子数；0 或省略为 60，超过 64 截为 64。"},
			"query":{"type":"string","description":"不区分大小写搜索标题和回复正文；省略不过滤。"}
		},"additionalProperties":false}`),
		definition("forum_read", "发现消息或分页读取指定消息正文。发现模式每条 content 最多 512 字节，外层 has_more 指剩余消息；正文模式须按 next_offset 续读至 has_more=false。完整 JSON 上限 4096 字节，实际正文可能小于 max_bytes。", `{"type":"object","properties":{
			"after_id":{"type":"integer","minimum":0,"description":"消息发现游标，默认 0，不得超过当前论坛 latest_id；用 next_id 续读。正文模式仅允许 0 或省略。"},
			"limit":{"type":"integer","minimum":0,"description":"发现模式消息数；0 或省略为 16，超过 64 截为 64。"},
			"all":{"type":"boolean","description":"true 查看全部公开消息；默认仅自己及 to 为空、* 或自己的消息。"},
			"thread_id":{"type":"integer","minimum":0,"description":"按线程筛选；0 或省略不筛选。"},
			"from":{"type":"string","description":"按已注册作者 ID 筛选；空值不过滤。"},
			"message_id":{"type":"integer","minimum":1,"description":"切换为正文分页模式；须为保留且可见的消息 ID，筛选条件仍生效。"},
			"offset":{"type":"integer","minimum":0,"description":"正文 UTF-8 字节偏移，默认 0；非零时必须配 message_id，且位于正文内字符边界。"},
			"max_bytes":{"type":"integer","minimum":0,"description":"正文页请求字节数；0 或省略为 1024，超过 4096 截为 4096。非零时必须配 message_id，以实际 next_offset 为准。"}
		},"allOf":[
			{"if":{"required":["message_id"]},"then":{"properties":{"after_id":{"const":0}}}},
			{"if":{"properties":{"offset":{"minimum":1}},"required":["offset"]},"then":{"required":["message_id"]}},
			{"if":{"properties":{"max_bytes":{"minimum":1}},"required":["max_bytes"]},"then":{"required":["message_id"]}}
		]}`),
		definition("forum_wait", "有界等待符合条件的消息，返回发现模式的正文前缀而非完整正文。不支持 message_id。timed_out/cancelled/peers_done 区分结果；超时或其他成员结束后继续有用工作，不无限等待。", `{"type":"object","properties":{
			"after_id":{"type":"integer","minimum":0,"description":"消息发现游标，默认 0，不得超过 latest_id。"},
			"limit":{"type":"integer","minimum":0,"description":"消息数；0 或省略为 16，超过 64 截为 64。"},
			"all":{"type":"boolean","description":"true 包含全部公开消息；默认仅自己及 to 为空、* 或自己的消息。"},
			"thread_id":{"type":"integer","minimum":0,"description":"线程筛选；0 或省略不筛选。"},
			"from":{"type":"string","description":"已注册作者 ID 筛选；空值不过滤。"},
			"timeout_seconds":{"type":"number","minimum":0,"description":"等待秒数，支持小数；0 或省略采用论坛配置，正数最少 1 毫秒，超过 120 截为 120 秒。"}
		}}`),
		definition("forum_roster", "查看已注册 Agent 的 ID、名字、阶段与运行状态，用实际 ID 路由论坛交流。", `{"type":"object","properties":{}}`),
		definition("forum_moderate", "仅已注册 id=moderator 且 stage=moderator 的论坛管理员可用。关闭/重开/置顶/取消置顶线程；关闭保留历史而非删除，动作和理由写入公开管理记录。", `{"type":"object","properties":{
			"thread_id":{"type":"integer","minimum":1,"description":"保留线程根或嵌套消息 ID。"},
			"action":{"type":"string","enum":["close","reopen","pin","unpin"],"description":"线程管理动作。"},
			"reason":{"type":"string","minLength":1,"description":"中文管理理由，去除首尾空白后非空且不超过 512 UTF-8 字节。"}
		},"required":["thread_id","action","reason"]}`),
		definition("forum_announce", "仅论坛管理员可用。向全体发布标记为公告的新帖，可置顶；协作建议不替代证据，不代其他 Agent 回答，也不计入阶段共识票。", `{"type":"object","properties":{
			"topic":{"type":"string","description":"公告标题，不超过 512 UTF-8 字节；可省略。"},
			"content":{"type":"string","minLength":1,"description":"公告正文，去除空白后非空且原文不超过 16384 UTF-8 字节。"},
			"pinned":{"type":"boolean","description":"同时置顶公告，默认 false。"}
		},"required":["content"]}`),
	}
}

func definition(name, description, parameters string) llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Name: name, Description: description, Parameters: json.RawMessage(parameters)}
}
