!{input}

!{review_state}

你现在处于【规划建图阶段】，不是执行审计阶段。

任务：先绘制 one-shot 审计地图，再创建执行阶段 todo 和本次要审计的文件范围。不要提交漏洞，不要结束审计，不要直接开始漏洞验证。

强制要求：首次启动审计必须先加载至少一个 skill。请先根据 Inventory 摘要、Interesting Paths、依赖/配置文件名和项目文件类型选择最相关 skill 并调用 load_skill。未加载 skill 前不要调用 review_state、list_files、read_file、search_content、todo_create、file_review_update 或 audit_plan_done。若不确定项目类型，优先加载通用 Web 审计 skill。

注意：文件排查状态默认是空的。上面的 Inventory 摘要和 Interesting Paths 只是文件地图参考，不代表这些文件都要审计。你必须自己选择本次 one-shot 要审计的文件，并通过 file_review_update 加入 file_review。

项目笔记要求：你必须主动调用 project_note_update 维护自由文本项目笔记。note 要像人工审计员做笔记一样详细，自己组织结构，记录项目架构、行为、登录认证、鉴权机制、攻击面、数据/状态流、关键文件角色、已知结论和待确认问题。不要只写摘要；每次读到新的架构/入口/认证/鉴权/业务行为信息后都应该更新。

必须优先基于上面的初始文件结构工作。系统已经枚举过文件，不要一开始就调用 list_files；只有当你需要按目录、深度或模式补充文件地图时才调用 list_files。你可以少量 read_file 读取依赖、配置、入口、路由、鉴权文件用于建图，也可以 search_content 搜索用于分类的关键词。

规划阶段必须完成：

0. 调用 load_skill 加载至少一个与当前项目相关的 skill。
1. 调用 project_note_update 初始化详细项目笔记，后续发现新信息要持续更新。
2. 项目画像：语言、项目类型、框架迹象、主要模块。
3. 文件地图：入口、鉴权、路由/控制器、业务服务、DAO/数据库、上传/文件操作、模板/插件、备份/导入导出、配置、低价值目录。
4. 信任边界和高价值资产。
5. Top 审计优先级，每项写清楚为什么高风险、要读哪些文件、成立条件、否定条件。
6. 调用 todo_create 创建执行阶段 todo。todo 必须包含具体文件、模块、入口、变量或审计点。
7. 调用 file_review_update 把执行阶段要审计的文件标记为 reviewing，并用 note 写明为什么纳入范围。支持 path 单文件、paths 多文件、dir/dirs + suffix/suffixes、pattern/patterns 批量加入。低价值目录通常不需要加入 file_review，除非你要明确记录跳过原因。
8. 规划完成后调用 audit_plan_done，audit_files 必须列出执行阶段要审计的具体文件路径。

下一步必须先调用 load_skill。工具调用必须且只能使用 <tool_call>{...}</tool_call> JSON。
