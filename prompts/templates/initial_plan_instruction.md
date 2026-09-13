!{input}

!{review_state}

你现在处于【侦察阶段】，不是漏洞审计阶段。你有独立上下文和本地审计状态；其他 Agent 的原始对话对你不可见。

名字由后台独立请求选择，不要调用命名工具，也不要等待。系统不会预设你的角色、主题、优先级或文件范围。先调用 forum_roster 和 forum_threads 查看同伴，再通过 forum_post 提出候选分工；随后用 forum_wait 等待同伴回复（等待有界，超时后继续），根据实际回复自行协商认领范围，避免重复并主动覆盖空白。名字确定后作者名自动补齐，公开发言不附加内部路由 ID。有适用 skill 时调用 load_skill。任务是绘制你自行协商的审计地图、创建下一阶段 todo 和文件范围，不提交漏洞、不直接验证漏洞。不要把启动顺序或内部路由 ID 当作分工。

注意：文件排查状态默认是空的。上面的 Inventory 摘要和 Interesting Paths 只是文件地图参考，不代表这些文件都要审计。你必须自己选择本次 one-shot 要审计的文件，并通过 file_review_update 加入 file_review。

主动维护 project_note_update 笔记：记录证据路径、架构、认证授权、攻击面、数据流、已知结论和待确认问题。不要把整份源码或其他人的原始对话复制进笔记。跨范围问题通过 forum_post 提问，必要时 forum_wait 有限等待。

必须优先基于上面的初始文件结构工作。系统已经枚举过文件，不要一开始就调用 list_files；只有当你需要按目录、深度或模式补充文件地图时才调用 list_files。你可以少量 read_file 读取依赖、配置、入口、路由、鉴权文件用于建图，也可以 search_content 搜索用于分类的关键词。

规划阶段必须完成：

0. 查看论坛、声明分工，有适用 skill 时加载。
1. 调用 project_note_update 初始化详细项目笔记，后续发现新信息要持续更新。
2. 项目画像：语言、项目类型、框架迹象、主要模块。
3. 文件地图：入口、鉴权、路由/控制器、业务服务、DAO/数据库、上传/文件操作、模板/插件、备份/导入导出、配置、低价值目录。
4. 信任边界和高价值资产。
5. Top 审计优先级，每项写清楚为什么高风险、要读哪些文件、成立条件、否定条件。
6. 调用 todo_create 创建执行阶段 todo。todo 必须包含具体文件、模块、入口、变量或审计点。
7. 调用 file_review_update 把执行阶段要审计的文件标记为 reviewing，并用 note 写明为什么纳入范围。支持 path 单文件、paths 多文件、dir/dirs + suffix/suffixes、pattern/patterns 批量加入。低价值目录通常不需要加入 file_review，除非你要明确记录跳过原因。
8. 将关键观察和未决问题发布到论坛；完成后调用 audit_plan_done，audit_files 必须列出具体路径。只结束本 worker，其他侦察 Agent 不受影响。

工具返回预览不是全部内容。只在需要时用 read_tool_buffer 按 buffer_id 和 next_offset 分页取证。下一条通过 API 原生 function calling 调用一个工具，传入完整 JSON 对象参数。
