# Java 安全审计 Skill

当目标代码是 Java / Spring / Spring Boot / MyBatis / Servlet / Struts / Spring MVC / JPA / Hibernate / Dubbo / RPC / Java Web 后端时加载这个 skill。

推荐与 `web-audit` 同时加载：

- `web-audit` 负责 Web 后台/管理端/业务逻辑/多组件联动审计。
- 本 skill 负责 Java 语言与框架层的高危漏洞：未授权、SQL 注入、反序列化、表达式注入、命令执行、权限绕过、对象注入、Jackson / Fastjson / Hessian / XStream / Kryo 等序列化风险。

审计重点：
- 未授权访问：接口是否缺少鉴权、方法级注解是否失效、Spring Security / Shiro / 自定义拦截器是否能被绕过、菜单权限和接口权限是否不一致、是否存在默认放行路径。
- SQL 注入：是否存在字符串拼接 SQL、XML 动态 SQL 拼接、`@Query` / `JdbcTemplate` / `EntityManager` / `PreparedStatement` 使用是否安全、`order by` / `group by` / `in` / `like` / 分页参数 / 表名字段名是否可控。请注意, JAVA的SQL注入漏洞需要额外查看XML文件进行分析, xml里面写的都是和数据库交互的代码，因此可以分析XML文件去挖掘SQL注入漏洞。必须进行XML文件分析.而且不能说搜索一下关键字就pass这块，需要对每个XML文件进行分析!
- 反序列化：是否存在把用户输入直接传入 `ObjectInputStream`、`XMLDecoder`、`XStream`、`Fastjson`、`Jackson`、`Hessian`、`Kryo`、`Protostuff`、`Dubbo`、`RMI` 等场景；是否允许外部可控类名、类型信息、白名单不严格、黑名单绕过、`readObject` / `readResolve` / `finalize` / `Comparator` / `TemplatesImpl` / gadget chain。

反序列化高危例子（需要重点盯）：

- `new ObjectInputStream(request.getInputStream()).readObject()`：直接从请求体反序列化对象。
- `XMLDecoder` / `XStream.fromXML()`：从外部输入加载 XML 对象。
- `JSON.parseObject(input, Object.class)`、`readValue(input, Object.class)`：允许不受控类型反序列化。
- `Fastjson` 开启或可绕过 autoType，或使用 `parseObject` 时目标类型可被污染。
- `HessianInput.readObject()`、`Kryo.readClassAndObject()`、`Protostuff` 反序列化外部输入。
- Dubbo / RMI / JMS / Redis / MQ / 缓存 / Cookie / Token / 字段回填中出现对象反序列化。
- `Serializable` 类中存在危险 `readObject`、`readResolve`、`writeReplace`、`toString`、`compareTo`、`equals`、`hashCode`、`Comparator` 链条。
- 还有很多其他的例子, 需要根据项目自行重点自我判断。反序列化是java非常常见的漏洞，重点审计。

检查要求：

- 必须从入口一路追踪到 sink，不能只看单个类就下结论。
- 对于未授权和权限绕过，要检查：拦截器、AOP、注解、Controller、Service、DAO、过滤器、网关、RPC 接口是否一致。
- 对于 SQL 注入，要检查：参数是否进入动态 SQL、表名/列名/排序字段/分页字段/拼接条件是否可控。
- 对于反序列化，要检查：输入来源、是否有白名单、是否限定目标类型、是否做完整性校验、是否能触发 gadget chain。
- 发现候选漏洞后，先用 `variable_review_update` 和 `flow_review_update` 追踪，再决定是否提交。
- 只有能证明攻击前提、利用链、实际影响、可重复性时才提交漏洞。
- JAVA里面可能这个模块因为其他模块引用导致利用链不完整,比如你发现了一个漏洞但是没人调用它(你尽量找过了).这个可能是其他引用他的代码的数据不在这个项目里面.JAVA的多复杂性的问题,所以你可以任然在利用链不完整但是有明确证据的情况下报漏洞.但是要额外说明,利用链不完整,没办法直到是从哪个路由来的.需要进一步核查.
- 优先审计sql注入
