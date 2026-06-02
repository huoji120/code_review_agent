!{input}

!{review_state}

系统已经在启动或切换工作区时枚举过文件，并把初始文件结构与排查状态放在上面的当前状态里。请直接基于这些文件结构规划跨文件审计顺序，不要一开始就调用 list_files；只有怀疑文件清单过期或确实需要刷新目录视图时，才调用 list_files。先用 todo_create 创建详细 todo，todo 必须包含具体文件/模块/入口/变量/审计点。后续每完成一个 todo 对应的审计动作或闭合结论，必须立刻调用 todo_update 设置 status:"completed"。不要因为已经发现一个漏洞就停下，继续围绕同一入口、同类代码和相邻模块深挖，尽可能一次找全更多问题。然后用 flow_review_update 建立至少一个需要追踪的入口到 sink 的调用链或数据流，再用 file_review_update 标记正在审计和已审计文件；在未读文件前不要轻易批量标记 reviewed。
