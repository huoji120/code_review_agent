# 下一代Agent代码审计
简短的来说，我不太喜欢现在开源的大部分的"代码审计"系统的复杂性和花里胡哨的UI.因此我开发了这套代码审计agent系统并且用于测评我的QWEN-EXO项目 

>《qwen-exo - 让 Qwen 在长任务里真正记住知识、反思错误，并跑得足够快》
https://github.com/huoji120/QWEN-EXO-booster

它具有以下特点
1. 完美适配QWEN-EXO,支持高速高效离离线部署,你只需要两台DGX就行.
2. agent跟团队一样在研究问题,模型之间会互相配合,他们会像一支专业团队一样,在内部地下论坛进行交流,记笔记,分工合作,分享成果,一起挖漏洞.
3. 不依赖第三方库,编译即可用,高效简洁,我非常讨厌乱七八糟的WEBUI以及花里胡哨的东西,简洁才好用.
4. 超大型项目实战,已经过大型项目测试,本项目为超大型项目审计而生.

# 相关截图
![img](img/3.png)

![img](img/4.png)

# 支持模型
我是自己用于本地离线代码审计的,建议使用QWENEXO作为本地推理架构,并且使用QWWEN-27B系列模型.
如果没条件,推荐使用deepseek-flash,不虚mythos.

# 技术介绍

## 多阶段分工合作,避免上下文污染
此agent使用多阶段分工合作,覆盖侦察->研究->利用阶段(公开版不带利用阶段),用于解决上下文污染的问题,具体参考问题:
《[2026]从模型注意力角度创建一个简单但高效的Agent》
https://key08.com/index.php/2026/08/10/3275.html

## 你的agent跟团队一样在研究问题
在测试QWEN-EXO代码审计的研究中,注意到agent数量和任务完成率成正比
![img](img/7.png)

模型生成返回 `response.incomplete`（常见于 THINK 达到 `max_output_tokens`）时，本次未完成的思考、回答和工具参数全部丢弃，回退到最近一次成功的工具结果/模型响应边界，自动重新请求最多 3 次；残缺工具调用不会执行。IRC 管理员消息会取消当前 Agent 的生成，下一轮从同一成功边界优先处理 IRC，避免无限 THINK 长时间占用而无法响应。

单个成员连续 3 次原生工具协议失败时，仅该 Agent 标记为失败并保留进度，其他成员继续运行，不再暂停整个团队。系统向论坛管理员发布“Agent 失败待诊断”，包含成员、阶段、回合、错误原因、最后工具与最近输出摘录；管理员可查看原因并通过 IRC 跟进。失败不等于阶段完成，不会跳过该成员的未完成工作。

流式 `timeout_seconds` 按实际 THINK、回答或工具参数输出刷新；仅 SSE 心跳不会延长等待。持续没有模型输出时取消请求并按未完成生成策略重试3次，仍失败仅标记当前成员并通知管理员。该超时不是总生成时长限制，持续输出的长推理不会因此被打断。

`/session`（或 `/sessions`）打开会话选择弹窗：↑↓、Home/End、PgUp/PgDn 或滚轮选择，Enter 恢复，Esc 关闭；运行/保存期间可浏览但不能恢复。`/restore` 不带参数也打开弹窗；支持完整路径、省略 `.json`、唯一文件名前缀/片段。多个匹配必须在弹窗中选择，不自动猜测。输入 `/restore <片段>` 后按 Tab 可补全唯一匹配，多个匹配打开候选列表。只补全路径，不修改或补造会话内容；恢复后输入 `go` 继续。

而这个居然是遵循《Scaling Law》的

![img](img/5.png)

因此我开发了一个多agent协作系统.负责模型之间的互相配合,他们会像你的团队一样,在内部地下论坛进行交流,记笔记,分工合作,一起解决你的问题
![img](img/6.png)

他们会独自分工,讨论问题,研究漏洞是否存在:
![img](img/8.png)

## 高效缓存,极致性价比
不做乱七八糟的提示词注入等破坏性的东西,缓存基本在97%-99%左右


![img](img/9.png)

## deepseek懒惰修复
在做这个项目中,我测试了deepseek,发现deepseek-v4.1-flash模型的上下文一旦长了就开始懒惰起来,会尽可能的想办法结束任务.因此我设计了一个投票结束系统,只有在所有agent都同意结束任务的情况下才能结束任务,否则agent会不工作.

有意思的是，deepseek的agent一个完成后，想投票关闭被拒绝后，会疯狂的骚扰其他的agent让他们结束自己的工作。因此对这块提示词做了优化，禁止一个agent去骚扰其他人，自己找活干。即便是重复的工作
普通（有限）模式下，关闭请求待定、被拒绝或超时后，每个 Agent 应继续**自己的审计工作**：自行选择其他未覆盖的文件、入口或模块，建立具体待办并读取源码、追踪新链路。不催其他成员投票，不反复请求关闭，也不靠重复复核同伴已有结论消磨时间。


## 无限猴子模式

无限模式下，审计模型没有 `end_audit` 结束工具，也不能投票收工。侦察完成后仍正常交接到审计；审计继续运行，直到用户暂停，或时间/token 任一预算耗尽。推荐在deepseek使用因为deepseek非常懒惰特别喜欢结束.导致大型项目没审计完就结束。

### 怎么开启

在你实际使用的配置文件（例如 `config.yaml` 或 `config_ds.yaml`）的 `agent:` 下加入：

```yaml
agent:
  infinite_mode: true
  budget_hours: 8
  budget_minutes: 0
  budget_tokens: 0
```

- 时间默认 **8小时**，token 默认不限；时间是 `budget_hours + budget_minutes`。
- **OR 关系**：时间到，或者累计 token 达到上限，任一发生就停止整个团队。
- 小时和分钟都为 `0` 表示时间不限；`budget_tokens: 0` 表示 token 不限。三项都为 `0` 才是不设预算上限。
- `infinite_mode: false` 是普通模式，仍使用团队共识结束；这几个预算上限不生效。

指定配置和目录启动：

```powershell
.\code-review-agent.exe -config config.yaml -dir "F:\待审计项目"
```

也可以启动程序后输入目录。无限模式会先弹出预算输入框，默认内容为：

```text
infinite 8 0 0
```

四项依次是 **模式、小时、分钟、token**。编辑后按 **Ctrl+S** 确认并开始，**Esc** 取消，不会启动。

| 输入 | 含义 |
| --- | --- |
| `infinite 8 0 0` | 最多8小时，token不限 |
| `infinite 2 30 10000000` | 最多2小时30分钟或1000万token，先到即停 |
| `infinite 0 0 10000000` | 时间不限，最多1000万token |
| `infinite 0 0 0` | 时间、token均不限，手动停止 |
| `normal 8 0 0` | 切回普通共识结束模式，预算不生效 |

停止状态下输入 **`/budget`** 可切换模式、调整预算；配置文件负责启动默认值，界面修改保存在会话中，不回写 YAML。运行中在对话框外**连续按两次 Esc** 确认暂停，等待成员完全停止后输入 **`go`** 继续。

## 示例漏洞列表(仅娱乐,让K3验证我自己没看过是不是真的)
### V8 riscv JS→Wasm wrapper ==1
riscv JS→Wasm wrapper ==1 vs 位测试 — ✅ 真的，证据很硬

对照同目录 x64（L458-459）和 arm64（L592-593）的同函数同位置：

```cpp
// x64:   testl(valuetype, 1); j(equal, &convert_param)     ← 位测试
// arm64: Tst(valuetype, 1);  B(&convert_param, eq)        ← 位测试
// riscv: Branch(&convert_param, eq, valuetype, Operand(1)) ← 等值比较
```

- ValueType 编码（value-type.h:229）TypeKindField 在低 2 位，bit0 区分数值/引用：上游用 Tst(valuetype, 1)，bit0=0（数值）→ 转换，bit0=1（引用）→ 直接存。
- riscv 改成 == 1：只有 raw 值恰为 1 的单一编码进 convert_param；所有 i64/f32/f64 数值参数（raw 为偶数 ≠ 1）全部掉进 handle_ref_param，不做转换直接把 tagged JS 值按引用类型存进 64 位槽 → wasm
  侧把指针位模式当数值用 / GC 把数值位模式当指针扫描，JS↔Wasm 双向类型混淆。返回值方向 L825 Branch(&return_kWasmRef, ne, valuetype, Operand(1)) 是同款错误的第二处。
- 最讽刺的是 riscv L598 保留了上游注释"kRefNull is not representable as a cmp Operand"——注释在说"没法用等值比较"，下一行却正是等值比较。移植时改坏的（或注入的），与上游语义明确偏离。

但暴露面：此文件需要 riscv64 架构 + V8_ENABLE_WASM_INTERPRETER（Drumbrake）双重门控，Chrome/Node 出货构建都不含。对真实用户≈零暴露；作为代码缺陷它是这批里最扎实的一个——如果该工作区是"找注入
bug"的靶场，这条值得算真发现，"high" 在该架构配置下名副其实，跨平台意义上则无人可达。

### V8 ValueDeserializer::ReadSharedObject 越界读
`ValueDeserializer::ReadSharedObject()` 从不可信序列化字节流中读取攻击者可控的 32 位 `shared_object_id`，**未做任何范围校验**直接索引 `SharedObjectConveyorHandles` 内部的 `std::vector<Handle<HeapObject>>`，唯一的检查是 release 构建下会被编译掉的 `DCHECK`。

| # | 条件 | 说明 |
|---|------|------|
| 1 | 嵌入方实现 `ValueDeserializer::Delegate::GetSharedValueConveyor` 并返回非空 conveyor | 无 delegate 时 `ReadSharedObject` 在 value-serializer.cc:2530 直接抛异常，路径安全 |
| 2 | 嵌入方对**攻击者可控的字节流**调用 `ValueDeserializer::ReadValue` | 字节流与 conveyor 必须可分离；若二者绑定原子传递（如 d8 Worker），攻击者无法篡改 id |
| 3 | 字节流 wire version ≥ 15 | 低版本 kSharedObject 按 unknown tag 处理 |
| 4 | release 构建 | debug 构建会先触发 `DCHECK(HasPersisted)` 终止，只能算 DoS 演示 |
| 5 | id 足够大使 `vector.data() + id*8` 落在未映射或攻击者可预测区域 | 小越界读行为依堆布局而定；大 id（如 0x08000000，偏移 1GiB）可稳定触发访问违例 |

**典型受影响场景**：嵌入方按 V8 API 契约保存 conveyor（文档明确要求 conveyor 生命周期覆盖后续反序列化），之后对来自外部的消息字节调 `ReadValue`——例如跨进程/跨线程消息总线中字节流可被第三方注入或篡改的宿主。


## 技术报告
具体技术细节和技术报告,我会在[key08.com](https://key08.com/)发布,尽请关注.
