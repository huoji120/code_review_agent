{
  "generated_at": "2026-09-13T02:23:34+08:00",
  "audit": {
    "ended": true,
    "summary": "[布丁] 【布丁/audit-1 审计总结（入口可达性与身份权限）】\n\n一、Todo 完成情况：9 个 todo 全部 completed。(1) 入口/分发可达性 index.php-\u003eWidget_Init-\u003eRouter/Do；(2) 后台用户管理入口 manage-users/profile/index + Widget_Users_Admin/Abstract_Users；(3) Security/Request 的 CSRF 与请求层；(4) 动作层权限覆盖 Metas/Contents/Plugins/Themes；(5) XmlRpc/IXR 入口；(6) Init/options 可写性；(7) Abstract/Comments、Abstract/Users 数据边界；(8) Db/Adapter/Mysql 与 Request 参数层；(9) 插件入口 EditorMD/Sticky/Links。\n\n二、已提交漏洞：finding#1（high, CWE-862）Links 插件 /action/links-edit 无鉴权，游客可未授权写/删友情链接并造成前台/后台存储型 XSS。根因 Do.php:79-82 对插件注册 action 无集中鉴权，Links 相对 Smilies/Action.php 漏了 protect。已与 recon-4、audit-4 交叉确认。\n\n三、文件覆盖：负责的 23 个首要文件全部 reviewed 并写中文 note；为闭合链路补读并 reviewed 了 Do.php、Menu.php、haxwiki_admin/common.php、extending.php、Links/Action.php、Links/Plugin.php、Ajax.php、Service.php、Feedback.php。范围内无 reviewing/unseen 的高价值入口文件遗留。\n\n四、变量/flow：记录 link name/url/image（已闭环）；flow 已转 finding 并删除。\n\n五、未确认线索（不报）：Menu.php:207 validate 基于原始 REQUEST_URI 严格比较，路径变体（PATH_INFO/大小写）理论上可跳过 pass()，但依赖 Web 服务器解析，本环境无法证实（audit-4 已按 high 提交）。extending.php 有 panelTable urlencode 白名单，简单绕过被阻断。\n\n六、结论：本维度确认 1 个可稳定利用的严重漏洞；其余内置动作鉴权覆盖齐全，未发现新的无条件未授权入口。\n\n[未命名 Agent] 【audit-2 跨文件数据传播与输入校验 审计总结】\n\n一、todo 完成情况：8 个 todo 全部 completed（install.php 反序列化可达性、Post/Abstract Contents 编辑链、Ajax/Service 鉴权粒度、Attachment/Tag 编辑、Login/Register 输入、Plugins/Edit+Upgrade、Common/Mysqli/Router 底层、插件/IXR/Author/backup/media）。\n\n二、文件排查覆盖：我首要负责的 22 个文件已全部 reviewed 并写了中文 note：install.php、haxwiki_admin/{backup,login,media,register}.php、usr/plugins/{HelloWorld,QiniuFile,UpyunFile}/Plugin.php、var/IXR/Message.php、var/Typecho/{Common,Router}.php、var/Typecho/Db/Adapter/Mysqli.php、var/Widget/{Abstract/Contents,Ajax,Login,Service,Upgrade}.php、var/Widget/Contents/{Post,Attachment}/Edit.php、var/Widget/Metas/Tag/Edit.php、var/Widget/Plugins/Edit.php、var/Widget/Users/Author.php。无 unseen/reviewing 剩余。\n\n三、变量/flow：已用 variable_review_update 记录关键变量（$input['name']/$attachment['title']），flow 队列（install 反序列化、附件标题 XSS）已闭环并 flow_review_delete 删除。\n\n四、提交漏洞：finding id=1（medium, CWE-79）附件编辑标题存储型 XSS。链路完整：contributor POST /action/contents-attachment-edit?do=update -\u003e Attachment/Edit.php:172/175 $attachment['title']=$input['name']（未净化，对比核心上传路径 Widget_Upload::getSafeName 会删引号/尖括号）-\u003e contents.title -\u003e haxwiki_admin/manage-medias.php:68 $attachments-\u003etitle() 原样 echo（Widget::__call 直接 echo）-\u003e 管理员打开媒体库执行任意 JS。\n\n五、已核实但判定不可利用/价值不足而未报（均有源码证据）：\n1) install.php:291 unserialize(__typecho_config)：站点 installed=1 时第62-65行直接 404 exit，正常不可达（诱饵）。\n2) var/Widget/Abstract/Users.php:120 getPageOffset 字符串拼接：唯一可控调用点 Users/Edit.php:263（原始 request-\u003euid），但 execute() pass('administrator')，仅管理员可利用，判低不报；Contents.php:234 调用方均传 int 或刷新后的 created，安全。\n3) Ajax.php/Service.php 各 action 有 pass 但缺 security-\u003eprotect()（CSRF），影响低。\n4) Login.php:75 referer 经 safeUrl 仅校验 scheme（不校验 host）=\u003e 登录后开放重定向（低）。\n5) Common.php::randString 用 rand() 生成 authCode salt（弱随机，需大量观测才可能利用）。\n6) QiniuFile/Plugin.php::uploadFile 未净化文件名（与附件标题 XSS 同类），但依赖插件被激活，条件性未报；UpyunFile 有 getSafeName 净化。\n7) 全站 SQL 均用 ? 占位符 + 延迟转义（Query.php quoteValue/quoteColumn），无其它 guest 可达注入；order/group 经 filterColumn 处理。\n\n六、为何可结束：本人 inventory 已 100% reviewed，且对“跨文件数据传播/输入校验”主题的代表性模式（外部入口-\u003e变量-\u003eDB/输出 sink、反序列化、SQL 拼接、XSS 输出、鉴权/CSRF 覆盖）均已通过针对性 search_content（expression/where 拼接/unserialize/Interface_Do/removeXSS）与逐文件阅读覆盖。其余非我负责的入口（Links 插件未授权写、Menu.php 鉴权绕过等）已由 audit-1/audit-3/audit-4 报告，无重复价值。\n\n[未命名 Agent] 【audit-3 危险sink与实际影响 审计总结】\n一、完成情况：10 个 todo 全部 completed；首要负责的 22 个文件全部 reviewed（含 var/Widget/{Do,Upload,Backup,Options,Logout,Stat,Plugins/List,Users/Edit,Contents/Attachment/Related,Abstract/Metas}.php、var/Typecho/{Cookie,Validate,Db/Adapter/Pdo}.php、var/IXR/Request.php、usr/plugins/{Links/Action,Links/Plugin,Smilies/Action,CodePrettify/Plugin,wangEditor/Plugin}.php、haxwiki_admin/{common,manage-comments,options-general,theme-editor}.php）；另交叉阅读了 Router/Parser、Widget.php、Menu.php、Security.php、Db/Query.php、install.php、Helper.php 以闭合调用链。\n二、已报告漏洞（1 个）：usr/plugins/Links/Action.php:83 友情链接插件 Links_Action 无 pass()/protect()，未认证可经 /action/links-edit?do=insert|update|delete|sort 任意增删改 links 表，且 Plugin.php:249-291 output_str() 未转义输出 → 存储型XSS（high，CWE-862）。利用链已用 variable_review(request-\u003eaction/actionTable) 与 flow 记录闭合，并经 verify_finding 复核。\n三、排除项（有明确理由）：install.php:291 __typecho_config 反序列化在 config 存在且 installed==1 时因 :63-65 直接 404 不可达；Upload.php 有 contributor pass+protect、文件名随机、扩展名白名单，无穿越/任意上传；Db 层 Query.php #param 延迟转义+PDO prepare，无 SQL 注入；Options.php execute 的 unserialize(theme/plugins/routingTable) 均来自管理员可写的 options 表，非攻击者可控；Backup/Users-Edit/Options-*/Themes-Edit 动作均有 administrator pass+protect；Smilies_Action 有 protect()；CodePrettify/wangEditor 插件无服务端危险 sink。\n四、变量/flow：request-\u003eaction/options.actionTable 已记录为 suspicious；Links 未授权 flow 已在提交后删除。\n五、剩余与移交：工作区其余大量文件属同伴范围；关键未闭环项为 Menu::getCurrentMenu（var/Widget/Menu.php:207/256-259 仅在 URL 精确匹配时 pass($access)，URL 变体可能 validate=false 跳过校验，但未登录会被 common.php:35-42 重定向 welcome.php，需低权已登录账号才能越权，已在论坛 thread 19 移交 audit-1/audit-4 复核 makeUriByRequest 归一化与实际 web server 行为）；XmlRpc 方法级鉴权归 audit-2。我范围内的高价值入口（插件 Action 分发、上传、备份、用户管理、DB 层、模板）与同类路径均已覆盖，继续深挖本范围剩余项不会增加有效安全覆盖。\n\n[小笼包] 【负责范围】audit-4（小笼包），重心：否定条件、边界条件、相邻同类路径。首要负责 22 个文件，已全部读完并标记 reviewed：haxwiki_admin/{file-upload,manage-medias,plugins,user}.php；usr/plugins/{ColorHighlight,Links,Smilies}/Plugin.php；var/HyperDown.php、var/IXR/Server.php、var/Typecho/Db.php、var/Typecho/Db/Query.php、var/Upgrade.php；var/Widget/{Abstract/Options.php、Comments/Edit.php、Contents/Attachment/Unattached.php、Feedback.php、Menu.php、Options/General.php、Register.php、Themes/Edit.php、User.php、Users/Profile.php}（另交叉读取 Do.php、Widget.php、Request.php、Users/Admin.php、Mysql 适配器、Links/Action.php、Smilies/Action.php）。9 个 todo 全部 completed。\n\n【已提交漏洞】finding #1（high，CWE-287）：var/Widget/Menu.php:207 后台鉴权绕过。$validate 依赖原始 REQUEST_URI 的 path 与菜单项 path 精确相等，用 PATH_INFO/./%2D 变体可使其为 false，跳过行258 user-\u003epass()；配合自设 Cookie __typecho_first_run=1 绕过 common.php 行35 的 welcome 重定向，未授权访问仅靠 Menu 兜底的后台 GET 页；以 Widget_Users_Admin::execute()（无 pass，直接列出全部用户）证实影响。变量排查($validate)与跨文件 flow 已闭合后删除。\n\n【交叉确认（属 audit-3）】usr/plugins/Links/Action.php 无 pass/protect，游客可 POST /action/links-edit 增删改友情链接；对比相邻 Smilies/Action.php 有 protect()，属漏检，已反馈且 audit-3 已报告。\n\n【已排查无问题】Db/Query.php（延迟转义+prepare 单次替换安全）、Db/Adapter/Mysql.php（引号加倍转义，GBK 亦安全）、User.php（hasLogin 严格===；simpleLogin 无调用方=死代码）、Comments/Edit.php、Feedback.php、Register.php、Users/Profile.php、Options/General.php、Abstract/Options.php、ColorHighlight/Plugin.php、HyperDown、IXR/Server.php、Upgrade.php。\n\n【未提交加固点】Options/General::removeShell 黑名单缺 phtml/php3/php7/phar（仅管理员可设）；Themes/Edit::editThemeFile 经未过滤 request-\u003eedit + themeFile trim(...,'./') 不阻中段 ../（管理员任意文件写，增量低）；Widget_User::pass 非法组字符串 null\u003c=N 恒真边界（用户组不可自改）。\n\n【剩余文件】我分配的首批 22 个文件全部 reviewed，无归属我的 unseen/reviewing 文件；其余为其他审计 Agent 负责的入口/安装/数据层/上传等，不阻塞我的结论。\n\n【结论】范围内提交 1 个 high（后台鉴权绕过），交叉确认 1 个 high（Links 未授权，audit-3 已报）；无更多高置信度严重问题。",
    "next_steps": "[布丁] 1) 若能获取 Web 服务器配置，验证 Menu.php:207 路径变体绕过是否真实可达。2) 复核 Links 插件启用状态以最终确认 finding#1 可达性。3) 关注其它插件动作注册方式与前端 \u003clinks\u003e 输出调用点。\n\n[未命名 Agent] 建议：1) 修复附件标题 XSS（输出转义 + 入库 removeXSS）。2) 对 QiniuFile 上传文件名补 getSafeName 净化。3) 统一 Ajax/Service 补 security-\u003eprotect()。4) 复核 Users/Edit getPageOffset 的 uid 参数化。5) 复核 QiniuFile/UpyunFile 是否激活以确认条件性项影响。\n\n[未命名 Agent] 建议团队复核并报告：1) Menu.php URL 变体导致的低权越权（若成立为 high/critical）；2) Widget_XmlRpc 游客可达方法中用 md5 密码、以及 mwNewMediaObject 上传的鉴权与文件类型；3) 前台 Widget_Feedback 评论提交入口的 XSS/频率/审核绕过。\n\n[小笼包] 1) 修复 finding#1：common.php 或各后台 widget 入口统一鉴权，并规范化 Menu 比对 path（解码 %XX、折叠 ./ 与连续斜杠），未匹配菜单项按未授权拒绝。2) audit-3 修复 Links/Action.php（补 pass+protect）。3) 排查所有仅靠 Menu 强制的后台 GET 页统一补鉴权。4) 加固 removeShell 扩展名黑名单与 Themes/Edit 路径穿越。"
  },
  "count": 4,
  "findings": [
    {
      "id": 1,
      "severity": "high",
      "title": "Links 插件 /action/links-edit 无鉴权，游客可写入友情链接并造成前台/后台存储型 XSS",
      "path": "usr/plugins/Links/Action.php",
      "line": 83,
      "evidence": "[布丁] 链路：index.php -\u003e Widget_Init -\u003e Typecho_Router::dispatch。var/Widget/Do.php::execute() 行60 取 request-\u003eaction，行72 array_merge($this-\u003e_map, unserialize(options-\u003eactionTable))，行79-82 当 class 存在且 implements Widget_Interface_Do 时直接 $this-\u003ewidget($widgetName)-\u003eaction();，无登录/权限/CSRF 任一校验。注册：usr/plugins/Links/Plugin.php:48 Helper::addAction('links-edit','Links_Action')，故 /action/links-edit 映射到 Links_Action。缺陷：usr/plugins/Links/Action.php action()（83-93）既无 $this-\u003euser-\u003epass() 也无 $this-\u003esecurity-\u003eprotect()；insertLink()（8-29）把 request-\u003efrom('name','url','sort','image','description','user') 直接 rows($link) 写库；deleteLink()/sortLink()/updateLink() 同样无鉴权。输出未转义：manage-links.php:53/54/58 直接 echo $link['name']、$link['url']、$link['image']；Plugin.php output_str()（284-288）把 {name}/{url}/{image}/{description} 直接 str_replace 进 HTML（256-260 模板 \u003ca href=\"{url}\" title=\"{title}\"\u003e{name}\u003c/a\u003e）。对照：相邻 usr/plugins/Smilies/Action.php 有 Helper::security()-\u003eprotect()，证明 Links 属漏校验而非全局设计。",
      "impact": "未认证攻击者可直接 POST /action/links-edit?do=insert\u0026name=\u003cscript\u003e...\u003c/script\u003e\u0026url=http://x 写入友情链接表；1) 管理员访问友情链接面板（extending.php?panel=Links/manage-links.php）时在管理员会话中执行存储型 XSS，可读取/外发 CSRF token（token=md5(secret\u0026authCode\u0026uid) 绑定 referer）进而伪造管理操作；2) 前台任意含 \u003clinks\u003e 短代码的文章/评论页会把恶意 name/url 原样渲染，形成对所有访客的持久 XSS。另 do=delete/sort/update 可未授权删改链接数据（完整性）。前置条件：Links 插件已启用。",
      "recommendation": "在 Links_Action::action() 起始加入 $this-\u003euser-\u003epass('administrator'); 与 $this-\u003esecurity-\u003eprotect();（参照 Smilies/Action.php）；对 name/url/image/description 在输出端统一 htmlspecialchars 转义，并在入库前做类型/URL 校验。",
      "cwe": "CWE-862"
    },
    {
      "id": 2,
      "severity": "medium",
      "title": "附件编辑标题未净化导致存储型 XSS（贡献者 -\u003e 管理员会话）",
      "path": "var/Widget/Contents/Attachment/Edit.php",
      "line": 175,
      "evidence": "[未命名 Agent] 入口：POST /action/contents-attachment-edit（Do.php _map -\u003e Widget_Contents_Attachment_Edit，execute() 要求 pass('contributor')，action() 有 protect()）。updateAttachment()（Attachment/Edit.php:172-185）：$input = $this-\u003erequest-\u003efrom('name','slug','description')（原始输入）；第175行 $attachment['title'] = $input['name']（无净化/转义）；$this-\u003eupdate($attachment, where cid=?) 写入 contents.title。对比核心上传路径 Widget_Upload::uploadHandle() 第95行会调用 getSafeName()（第66-75行 str_replace(array('双引号','小于','大于'),'',name)）净化文件名，编辑路径缺失该校验。sink：haxwiki_admin/manage-medias.php:68 用 \u003c?php $attachments-\u003etitle(); ?\u003e 原样输出（Typecho_Widget::__call 在 Widget.php:363-371 直接 echo $this-\u003etitle，__get('title') 返回原始 DB 行值，无 htmlspecialchars）；第74行 parentPost-\u003etitle() 同样原样输出。",
      "impact": "拥有 contributor 权限（或为附件作者）的低权限用户提交 name=\u003cimg src=x onerror=...\u003e 后，管理员访问媒体库（manage-medias.php）即触发任意 JavaScript，可窃取管理员 Cookie/CSRF token，实现权限提升与后台接管（存储型 XSS）。",
      "recommendation": "输出侧：manage-medias.php 等模板对 $attachments-\u003etitle() 使用 htmlspecialchars 转义。输入侧：在 updateAttachment()/writePost() 入库前用 Typecho_Common::removeXSS 或与 getSafeName 一致的净化逻辑处理 name/title，保证上传路径与编辑路径校验一致。",
      "cwe": "CWE-79"
    },
    {
      "id": 3,
      "severity": "high",
      "title": "友情链接插件 Links_Action 未授权操作导致任意增删改链接及存储型XSS",
      "path": "usr/plugins/Links/Action.php",
      "line": 83,
      "evidence": "[未命名 Agent] 1) 路由：install.php:369 定义 do 路由 url=/action/[action:alpha] → Widget_Do；var/Typecho/Router/Parser.php:60 默认 alpha 正则 ([_0-9a-zA-Z-]+) 含连字符，故 /action/links-edit 可匹配。\\n2) 分发：var/Widget/Do.php:72 array_merge(_map, unserialize(options-\u003eactionTable))，74-82 行命中 actionTable['links-edit']='Links_Action' 后直接调用 action()，全程无登录/CSRF 校验（Typecho_Widget::widget() 实例化时即执行 execute()，Widget.php:221）。\\n3) 注册：usr/plugins/Links/Plugin.php:48 activate() 中 Helper::addAction('links-edit','Links_Action')（var/Helper.php:203 写入 options.actionTable）。\\n4) 漏洞点：usr/plugins/Links/Action.php:83-93 action() 仅按 do=insert/update/delete/sort 分派，无 $this-\u003euser-\u003epass() 也无 $this-\u003esecurity-\u003eprotect()；对比同类 usr/plugins/Smilies/Action.php:115-118 调用了 protect()，var/Widget/Upload.php:420 有 pass('contributor',true)+protect()。\\n5) 写入：insertLink(Action.php:14-18) 用 request-\u003efrom('name','url','sort','image','description','user') 直接 insert 到 links 表；updateLink/deleteLink/sortLink 同理。\\n6) XSS sink：usr/plugins/Links/Plugin.php:249-291 output_str() 用 str_replace 把 {name}/{url}/{title}/{description}/{image} 直接替换进 HTML 模板（\u003cli\u003e\u003ca href=\"{url}\" title=\"{title}\"\u003e{name}\u003c/a\u003e\u003c/li\u003e），未 htmlspecialchars。",
      "impact": "当 Links 插件激活时，任意未认证访问者可向 /action/links-edit?do=insert|update|delete|sort 提交参数，直接增删改 links 表（无需登录/CSRF）；在主题或含 \u003clinks\u003e 标签的页面渲染链接时，注入的 name/url/description 中的 HTML/JS 原样输出，形成存储型 XSS，可窃取管理员 Cookie（Typecho_Cookie::set 未设 HttpOnly）接管后台。",
      "recommendation": "在 Links_Action::action() 开头加 $this-\u003euser-\u003epass('administrator') 与 $this-\u003esecurity-\u003eprotect()；output_str() 输出前对 name/url/description/image 做 htmlspecialchars 转义；Cookie 增加 HttpOnly。",
      "cwe": "CWE-862"
    },
    {
      "id": 4,
      "severity": "high",
      "title": "Typecho 后台鉴权绕过：URL 路径变体致 Widget_Menu::$validate=false 跳过 pass，未授权访问后台页面（manage-users.php 泄露用户信息）",
      "path": "var/Widget/Menu.php",
      "line": 207,
      "evidence": "[小笼包] 控制点 var/Widget/Menu.php::execute() 行206 `$validate = true;`，行207 `if ($urlParts['path'] != $currentUrlParts['path']) { $validate = false; }`，仅当 $validate 为 true 时行256-258 `$this-\u003euser-\u003epass($access)` 才会执行。$currentUrl 由行159 `$this-\u003erequest-\u003emakeUriByRequest()` 得到，而 Typecho_Request::getRequestUri()（var/Typecho/Request.php:441-442）直接返回原始 `$_SERVER['REQUEST_URI']`（不做路径规范化）。\n触发链：haxwiki_admin/manage-users.php 行2 include common.php；common.php 行23 `Typecho_Widget::widget('Widget_Menu')-\u003eto($menu)`，Typecho_Widget::widget()（var/Typecho/Widget.php:219-221）构造后立即调用 `execute()`；当请求路径与菜单项路径不一致时 $validate=false，行258 的 pass('administrator') 被跳过；common.php 行30 getCurrentMenu() 因 _currentParent 保持默认 1 返回默认「概要」项（非空）；common.php 行35 对未登录访客的重定向可被请求方自设 Cookie `__typecho_first_run=1` 绕过；随后 manage-users.php 渲染 Widget_Users_Admin，其 execute()（var/Widget/Users/Admin.php:79-98）无任何 pass 检查，直接 `$this-\u003edb-\u003efetchAll($select, array($this,'push'))` 输出全部用户。\n示例请求：`GET /haxwiki_admin/manage-users.php/x` 或 `GET /haxwiki_admin/./manage-users.php` 或对 `-` 使用 %2D 编码，并附带 `Cookie: __typecho_first_run=1`（服务器会把该路径规范化后仍执行 manage-users.php，而 REQUEST_URI 保留变体，致使路径比较不相等）。",
      "impact": "未授权（游客）可绕过后台鉴权访问仅靠 Menu 强制点保护的后台 GET 页面。以 manage-users.php 为例可读取全部用户账号信息（用户名、邮箱、昵称、注册信息），并可同理访问 manage-posts.php、manage-comments.php、options-general.php、plugins.php、backup.php 等页面，导致后台敏感信息泄露与攻击面暴露。属认证绕过/越权访问。",
      "recommendation": "1) 不依赖菜单 URL 精确匹配做鉴权：在 common.php 或各后台 widget 入口按页面所需权限统一调用 $user-\u003epass()。2) Menu::execute 比较前先规范化 $currentUrlParts['path']（解码 %XX、折叠连续斜杠与 ./、与路由一致的 pathinfo 解析）后再与菜单项 path 比较。3) 未匹配任何菜单项时按未授权处理并拒绝，而非回退默认项。",
      "cwe": "CWE-287"
    }
  ],
  "todos": [
    {
      "id": 1,
      "title": "[布丁] 审计入口与分发可达性：index.php -\u003e var/Widget/Init.php -\u003e var/Typecho/Router.php 分发链，确认 /action/[action:alpha] 是否无集中鉴权、哪些 action 是游客/低权限可达入口",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 2,
      "title": "[布丁] 审计后台用户管理入口 manage-users.php/profile.php/index.php 与 Widget_Users_Admin/Abstract_Users：确认这些页面无自有 pass，仅靠 Menu.getCurrentMenu 的菜单匹配硬校验（路径匹配时才 pass）；Widget_Users_Admin::execute 无 pass、仅参数化查询。属设计依赖，未发现无条件绕过",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 3,
      "title": "[布丁] 审计 Security.php/Request.php：token=md5(secret\u0026authCode\u0026uid) 绑 referer，Request 构造 array_merge($_POST,$_GET)；protect() 依赖 referer 属常规 CSRF 设计，未构成独立严重漏洞",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 4,
      "title": "[布丁] 审计动作层权限：Metas/Category/Edit(action 有 protect、execute 有 pass editor)、Contents/Page/Edit(pass editor+protect)、Plugins/Config(pass administrator)、Themes/Files(pass administrator+路径正则)、Contents/Attachment/Admin(pass editor 过滤)，均具备校验，未发现缺失",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 5,
      "title": "[布丁] 审计 XmlRpc/IXR：Widget_XmlRpc::action 建 IXR_Server，各方法经 checkAccess(用户名,密码,level) 校验；allowXmlRpc==0 直接 404；IXR/Base64、IXR/Value 仅序列化辅助，无 RCE/反序列化入口",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 6,
      "title": "[布丁] 审计 Widget_Init/options 可写性：Init.php 仅在 installed 为空时 set installed=1，无外部可控参数；options 写入均经 admin 选项 action（pass administrator+protect），未发现未授权写 secret/actionTable 的入口",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 7,
      "title": "[布丁] 审计 Abstract/Comments.php 与 Abstract/Users.php：Comments 仅 filter/push/输出；Users 的 nameExists/mailExists/screenNameExists 均参数化；getPageOffset 拼 {$offset} 但调用方仅传 DB 派生常量且需 administrator，未成可利用注入",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 8,
      "title": "[布丁] 审计 Mysql.php/Request.php：quoteValue 手写转义（' 和 \\），Query 用延迟参数占位绑定；Request.__construct array_merge($_POST,$_GET) 使 GET 覆盖 POST（对 action 判断有影响但非独立漏洞）；未发现可稳定利用的 SQL 注入",
      "status": "completed",
      "priority": "medium"
    },
    {
      "id": 9,
      "title": "[布丁] 审计插件入口 EditorMD/Sticky/Links：确认 Links_Action::action 无 pass/protect（已报高危），Sticky/EditorMD 无用户可达 action",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 10,
      "title": "[未命名 Agent] install.php:291 unserialize 不可达（installed=1 守卫 404），判定为诱饵",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 11,
      "title": "[未命名 Agent] Post/Edit.php + Abstract/Contents.php 已审：pass('contributor')+protect；expression(int_value) 经 intval 安全；getPageOffset 拼接仅 Users/Edit 传原始 uid（admin-only，未报）",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 12,
      "title": "[未命名 Agent] Ajax.php + Service.php 已审：各 handler 有 pass 但缺 security-\u003eprotect()；影响低（editorResize 不回显、sendPing 受限），未报",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 13,
      "title": "[未命名 Agent] 审计 Attachment/Edit.php（第178行 unserialize 属 DB 数据不可控）+ Metas/Tag/Edit.php（有 pass/protect、参数化安全）；确认 updateAttachment 标题未净化 -\u003e manage-medias.php 存储型 XSS（已提交）",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 14,
      "title": "[未命名 Agent] Login.php + admin/login.php + register.php 已审：protect 齐全，输出 htmlspecialchars；referer 经 safeUrl 仅校验 scheme =\u003e 登录后开放重定向（低，未报）",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 15,
      "title": "[未命名 Agent] Plugins/Edit.php + Upgrade.php 已审：action 均 pass('administrator')+protect()；plugin 名 filter('slug')；Upgrade $package 不可控，无越权",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 16,
      "title": "[未命名 Agent] Common.php/Mysqli.php/Router.php 已审：quoteValue/quoteColumn 转义到位；randString 用 rand() 弱随机（authCode salt）；safeUrl 仅校验 scheme；Router 正则表来自 DB，无直接注入",
      "status": "completed",
      "priority": "medium"
    },
    {
      "id": 17,
      "title": "[未命名 Agent] HelloWorld/QiniuFile/UpyunFile + IXR/Message + Users/Author + backup/media 已审：UpyunFile 有 getSafeName 净化；QiniuFile 未净化文件名（需激活，条件性）；IXR 无 XXE；Author 参数化；backup/media 为模板",
      "status": "completed",
      "priority": "medium"
    },
    {
      "id": 18,
      "title": "[未命名 Agent] 审计 usr/plugins/Links/Action.php 的 action() 是否缺失鉴权，导致未授权写与存储型XSS",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 19,
      "title": "[未命名 Agent] 审计 var/Widget/Do.php 动作分发无统一鉴权，核对各 action 的 pass/protect 覆盖",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 20,
      "title": "[未命名 Agent] 审计 var/Widget/Upload.php 上传流程：文件类型校验、路径拼接、扩展名绕过、写入位置与可达性",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 21,
      "title": "[未命名 Agent] 审计 var/Widget/Backup.php 导出与导入流程的危险 sink",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 22,
      "title": "[未命名 Agent] 审计 var/Widget/Options.php execute 反序列化与配置写入路径",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 23,
      "title": "[未命名 Agent] 审计用户管理 Edit.php 的越权与密码修改路径",
      "status": "completed",
      "priority": "medium"
    },
    {
      "id": 24,
      "title": "[未命名 Agent] 审计 SQL sink：var/Typecho/Db/Adapter/Pdo.php 与 Db/Query.php 的转义绕过可能性",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 25,
      "title": "[未命名 Agent] 审计 XMLRPC 入口 IXR/Request.php 与 Widget/XmlRpc.php 游客可达写入",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 26,
      "title": "[未命名 Agent] 审计 usr/plugins/Smilies/Action.php 与 CodePrettify/wangEditor 插件的 action 与前端注入",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 27,
      "title": "[未命名 Agent] 审计后台页面 common.php manage-comments.php options-general.php theme-editor.php 的鉴权与模板输出",
      "status": "completed",
      "priority": "medium"
    },
    {
      "id": 28,
      "title": "[小笼包] 审计入口与分发鉴权（Do.php 已确认无集中鉴权；Router/Init 由 audit-1 覆盖）",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 29,
      "title": "[小笼包] 审计 haxwiki_admin/file-upload.php 与 manage-medias.php 的上传/媒体操作鉴权、文件类型与路径边界（含否定分支）",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 30,
      "title": "[小笼包] 审计 haxwiki_admin/plugins.php 与 user.php 后台页面的权限校验否定分支与参数处理",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 31,
      "title": "[小笼包] 审计 var/Typecho/Db/Query.php 与 var/Typecho/Db.php 的 SQL 转义边界、#param# 占位符与 rows/order/group 拼接",
      "status": "completed",
      "priority": "critical"
    },
    {
      "id": 32,
      "title": "[小笼包] 审计 var/Widget/Menu.php getCurrentMenu 的 $validate 否定分支与 URL 变体绕鉴权（末尾斜杠/大小写/query）",
      "status": "completed",
      "priority": "critical"
    },
    {
      "id": 33,
      "title": "[小笼包] 审计 var/Widget/Comments/Edit.php 与 Feedback.php 的评论/反馈权限否定条件与越权编辑、删除、审核",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 34,
      "title": "[小笼包] 审计 var/Widget/Register.php 注册开关否定分支与 var/Widget/Users/Profile.php、var/Widget/User.php 资料修改越权",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 35,
      "title": "[小笼包] 审计 var/Widget/Contents/Attachment/Unattached.php、var/Widget/Abstract/Options.php、var/Widget/Options/General.php 的附件归属/选项写入边界",
      "status": "completed",
      "priority": "high"
    },
    {
      "id": 36,
      "title": "[小笼包] 审计 usr/plugins/{ColorHighlight,Links,Smilies}/Plugin.php 与 var/IXR/Server.php、var/HyperDown.php、var/Upgrade.php、var/Widget/Themes/Edit.php 的鉴权/解析/覆盖写边界",
      "status": "completed",
      "priority": "high"
    }
  ],
  "files": {
    "reviewed": [
      {
        "path": "config.inc.php",
        "status": "reviewed",
        "note": "[布丁] 站点配置：后台目录 /haxwiki_admin/、DB 凭证明文；include_path 含 var 与 usr/plugins。"
      },
      {
        "path": "haxwiki_admin/backup.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 模板：上传/服务器恢复表单，file 名逐项 echo（备份文件名由系统生成）；实际导出/导入逻辑在 Widget_Backup。"
      },
      {
        "path": "haxwiki_admin/common.php",
        "status": "reviewed",
        "note": "[布丁] 后台公共 include；Menu.getCurrentMenu() 为后台页唯一硬鉴权点（路径匹配时才 pass）。\n[未命名 Agent] 后台统一入口，鉴权依赖 Menu::getCurrentMenu() 的 pass($access)；未登录会重定向 welcome.php。\n[小笼包] 后台公共初始化：载入 config、Widget_Init、Options/User/Security/Menu，调用 menu-\u003egetCurrentMenu()。仅对 administrator 做升级检查(pass('administrator',true))，未对页面做全局鉴权。"
      },
      {
        "path": "haxwiki_admin/extending.php",
        "status": "reviewed",
        "note": "[布丁] 插件面板入口：panel 必须 urlencode 命中 panelTable['file'] 才 include，阻断简单 case/编码绕过。"
      },
      {
        "path": "haxwiki_admin/file-upload.php",
        "status": "reviewed",
        "note": "[小笼包] 撰写页附件面板片段，受 __TYPECHO_ADMIN__ 保护；按 $post/$page 的 cid 实例化 Attachment_Related/Unattached。无直接漏洞。"
      },
      {
        "path": "haxwiki_admin/header.php",
        "status": "reviewed",
        "note": "[小笼包] 后台头模板，仅输出静态资源与标题，无鉴权逻辑。"
      },
      {
        "path": "haxwiki_admin/index.php",
        "status": "reviewed",
        "note": "[布丁] 后台概要页：include common.php 靠 Menu 鉴权。"
      },
      {
        "path": "haxwiki_admin/login.php",
        "status": "reviewed",
        "note": "[未命名 Agent] remember_name/referer 均 htmlspecialchars 输出，无 XSS。"
      },
      {
        "path": "haxwiki_admin/manage-comments.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 表单页；数据来自 Widget_Comments_Admin（有 editor pass 校验），无输出转义问题发现。"
      },
      {
        "path": "haxwiki_admin/manage-medias.php",
        "status": "reviewed",
        "note": "[小笼包] 媒体管理模板，渲染 Widget_Contents_Attachment_Admin；删除/清理动作走 /action/contents-attachment-edit 并由该 widget 自检。"
      },
      {
        "path": "haxwiki_admin/manage-users.php",
        "status": "reviewed",
        "note": "[布丁] 用户管理页：靠 Menu.getCurrentMenu 路径匹配硬校验；Widget_Users_Admin 无自有 pass。未发现无条件绕过。\n[小笼包] 后台用户列表页，include common/header/menu 后渲染 Widget_Users_Admin，无内联鉴权，依赖 Menu 强制点。"
      },
      {
        "path": "haxwiki_admin/media.php",
        "status": "reviewed",
        "note": "[未命名 Agent] $attachment-\u003eattachment-\u003ename()/url()/size 原样 echo（name 经核心 getSafeName 净化），title 类输出归 manage-medias.php 已报告。"
      },
      {
        "path": "haxwiki_admin/menu.php",
        "status": "reviewed",
        "note": "[小笼包] 后台导航模板，调用 menu-\u003eoutput()，无鉴权逻辑。"
      },
      {
        "path": "haxwiki_admin/options-general.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 仅渲染 Widget_Options_General-\u003eform()，动作端点有 pass('administrator')。"
      },
      {
        "path": "haxwiki_admin/plugins.php",
        "status": "reviewed",
        "note": "[小笼包] 后台插件列表页，渲染 Widget_Plugins_List（无 pass 检查，依赖 Menu 强制点）；启用/禁用动作走 /action/plugins-edit 由 Widget_Plugins_Edit::action() pass('administrator') 自检。"
      },
      {
        "path": "haxwiki_admin/profile.php",
        "status": "reviewed",
        "note": "[布丁] 个人资料页：靠 Menu 鉴权，表单提交到 /action/users-profile（有 protect）。"
      },
      {
        "path": "haxwiki_admin/register.php",
        "status": "reviewed",
        "note": "[未命名 Agent] remember_name/mail htmlspecialchars 输出；allowRegister 守卫，无 XSS。"
      },
      {
        "path": "haxwiki_admin/theme-editor.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 渲染 Widget_Themes_Files（pass('administrator')），textarea 输出主题源码；仅管理员可达。"
      },
      {
        "path": "haxwiki_admin/user.php",
        "status": "reviewed",
        "note": "[小笼包] 后台用户表单页，渲染 Widget_Users_Edit-\u003eform()。页面无自身鉴权，依赖 widget 内 pass('administrator')与 Menu 强制点。"
      },
      {
        "path": "index.php",
        "status": "reviewed",
        "note": "[布丁] 前台入口：include config -\u003e Widget_Init -\u003e Plugin begin -\u003e Router::dispatch -\u003e end。"
      },
      {
        "path": "install.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 第291行 unserialize(__typecho_config Cookie) 位于 else(?start) 分支；前置守卫 line62-65 要求 installed 非空且 !=1，站点已安装(installed=1)则 404 exit =\u003e 不可达（诱饵）。\n[未命名 Agent] config 存在且 installed==1 时第63-65行直接 404 exit；第291行 __typecho_config 反序列化点在本实例不可达（除非 config 缺失或 installed!=1）。"
      },
      {
        "path": "usr/plugins/CodePrettify/Plugin.php",
        "status": "reviewed",
        "note": "[未命名 Agent] header/footer 输出 css/js URL，$style 来自管理员配置且经 enum 校验，无注入。"
      },
      {
        "path": "usr/plugins/ColorHighlight/Plugin.php",
        "status": "reviewed",
        "note": "[小笼包] 代码高亮插件：parseCallback 对内容 htmlspecialchars 转义、language 正则校验，无可利用 XSS。"
      },
      {
        "path": "usr/plugins/EditorMD/Plugin.php",
        "status": "reviewed",
        "note": "[布丁] Markdown 编辑器插件：仅前端 JS 注入与前台解析 hook，无服务端鉴权/写入入口。"
      },
      {
        "path": "usr/plugins/HelloWorld/Plugin.php",
        "status": "reviewed",
        "note": "[未命名 Agent] render() 用 htmlspecialchars 输出插件配置，安全。"
      },
      {
        "path": "usr/plugins/Links/Action.php",
        "status": "reviewed",
        "note": "[布丁] action() 无 pass/protect，未授权写/删/改友情链接（已报高危）。\n[未命名 Agent] Links_Action::action() 无 pass()/protect()；insertLink/updateLink/deleteLink/sortLink 直接读写 links 表；入口 /action/links-edit?do=... 游客可达（插件激活时）。发现未授权写+存储型XSS。\n[小笼包] 【重点】action() 无 pass()/无 security-\u003eprotect()，execute 前也不做鉴权；Do.php 直接调用。未授权可 insert/update/delete/sort 友情链接表。通知消息 _t('...\u003ca href=%s\u003e%s\u003c/a\u003e',url,name) 未转义，可形成对管理员的 XSS。属 audit-3 范围，已交叉确认。"
      },
      {
        "path": "usr/plugins/Links/Plugin.php",
        "status": "reviewed",
        "note": "[布丁] 注册 addAction('links-edit')/addPanel；output_str() 行284-288 未转义拼接 {name}/{url}/{image}，前台 XSS 点。\n[未命名 Agent] activate() 中 Helper::addAction('links-edit','Links_Action') 注册动作；output_str(249-291) str_replace 未转义输出 name/url 等 -\u003e 存储型XSS sink。\n[小笼包] 友情链接插件定义：activate 注册面板+action('links-edit'-\u003eLinks_Action)+内容解析钩子。output_str 将链接字段(name/url/image 等)原样替换进 HTML 模板，parseCallback 以内容中的 \u003clinks\u003e 模式($matches[3])为模板输出——存在存储型 XSS 候选(需内容作者可控)。"
      },
      {
        "path": "usr/plugins/Links/manage-links.php",
        "status": "reviewed",
        "note": "[布丁] 行53/54/58 直接 echo $link name/url/image 未转义，存储型 XSS 渲染点。"
      },
      {
        "path": "usr/plugins/QiniuFile/Plugin.php",
        "status": "reviewed",
        "note": "[未命名 Agent] uploadFile() 仅 explode('.') 取扩展名校验，未调用 getSafeName 净化 =\u003e 若插件激活，原始文件名(含引号/尖括号)会存入 attachment name/title，与附件标题 XSS 同类（需插件激活，条件性）。"
      },
      {
        "path": "usr/plugins/Smilies/Action.php",
        "status": "reviewed",
        "note": "[未命名 Agent] Smilies_Action::action() 调用了 Helper::security()-\u003eprotect()（对比 Links 缺失）；仅扫描表情目录返回 JSON，无危险写。\n[小笼包] scanfolders 只读目录列举并 JSON 输出；action() 有 Helper::security()-\u003eprotect()。无鉴权缺失。"
      },
      {
        "path": "usr/plugins/Smilies/Plugin.php",
        "status": "reviewed",
        "note": "[小笼包] 表情插件定义；注册 action('smilies')。相邻 Action.php 有 protect()（对比 Links 缺失）。"
      },
      {
        "path": "usr/plugins/Sticky/Plugin.php",
        "status": "reviewed",
        "note": "[布丁] 置顶插件：无用户可达 action；sticky() 查询参数化。"
      },
      {
        "path": "usr/plugins/UpyunFile/Plugin.php",
        "status": "reviewed",
        "note": "[未命名 Agent] uploadHandle 第137行调用自带 getSafeName(\u0026$name) 净化文件名（删除引号/尖括号），与核心一致，安全。"
      },
      {
        "path": "usr/plugins/wangEditor/Plugin.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 仅注入后台编辑器 JS，uploadURL 走 security()-\u003eindex，无服务端危险 sink。"
      },
      {
        "path": "var/HyperDown.php",
        "status": "reviewed",
        "note": "[小笼包] Markdown 解析器。hook 机制 call_user_func_array(行246) 仅调用内部注册回调；开启 enableHtml(true) 并对标签/属性做白名单清洗，未发现直接可利用的 XSS 或代码执行（内容作者为 contributor+，风险有限）。"
      },
      {
        "path": "var/IXR/Base64.php",
        "status": "reviewed",
        "note": "[布丁] IXR Base64 辅助类，仅 base64_encode。"
      },
      {
        "path": "var/IXR/Message.php",
        "status": "reviewed",
        "note": "[未命名 Agent] xml_parser_create（expat）解析 XML-RPC，未启用外部实体，无 XXE。"
      },
      {
        "path": "var/IXR/Request.php",
        "status": "reviewed",
        "note": "[未命名 Agent] IXR 客户端请求构造器，仅拼接 XML，无危险 sink。"
      },
      {
        "path": "var/IXR/Server.php",
        "status": "reviewed",
        "note": "[小笼包] IXR XML-RPC 基类：call() 走 callbacks 白名单(hasMethod 校验)，call_user_func_array 仅对注册回调。非用户可控方法名。未见直接漏洞。"
      },
      {
        "path": "var/IXR/Value.php",
        "status": "reviewed",
        "note": "[布丁] IXR 值序列化辅助，仅生成 XML，无 RCE。"
      },
      {
        "path": "var/Typecho/Common.php",
        "status": "reviewed",
        "note": "[未命名 Agent] randString 用 rand()（弱随机）生成 authCode salt；safeUrl 仅校验 scheme；removeXSS 为传统黑名单过滤（评论/feedback 使用）；hash/hashValidate 自定义算法。"
      },
      {
        "path": "var/Typecho/Cookie.php",
        "status": "reviewed",
        "note": "[未命名 Agent] get() 在 cookie 不存在时回退 $_POST（已知行为，需资质校验才可被利用）；set() 用 setrawcookie 未加 httponly；is_array 防护。本身无独立漏洞。"
      },
      {
        "path": "var/Typecho/Db.php",
        "status": "reviewed",
        "note": "[小笼包] Db 封装：query() 对 Typecho_Db_Query 调用 prepare() 做占位符替换，然后交适配器执行。selectDb 连接池随机选择。未发现注入点。"
      },
      {
        "path": "var/Typecho/Db/Adapter/Mysql.php",
        "status": "reviewed",
        "note": "[布丁] quoteValue 手写转义，Query 占位绑定；未发现可稳定利用注入。\n[小笼包] MySQL 适配器：quoteValue 采用引号加倍+反斜杠加倍(ANSI 安全，非 mysql_real_escape_string；对 GBK 注入同样安全)。parseSelect 拼接字段/表/where/group/having/order/limit。"
      },
      {
        "path": "var/Typecho/Db/Adapter/Mysqli.php",
        "status": "reviewed",
        "note": "[未命名 Agent] query 直接执行 SQL，quoteValue/quoteColumn 转义；无自身拼接漏洞。"
      },
      {
        "path": "var/Typecho/Db/Adapter/Pdo.php",
        "status": "reviewed",
        "note": "[未命名 Agent] query() 使用 PDO prepare/execute，配合 Query 的 #param 延迟转义，参数化绑定；无 SQL 注入。"
      },
      {
        "path": "var/Typecho/Db/Query.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 延迟转义 #param:N# 占位 + PDO prepare 绑定；filterColumn 过滤列名；未发现 SQL 注入。\n[小笼包] SQL 构建器：值经 quoteValue 延迟转义为 #param:N#，prepare() 用 preg_replace_callback 以 adapter-\u003equoteValue 替换(单次替换，不重扫，安全)；列名/order/group 走 filterColumn 加反引号。quoteValues 支持数组(IN)。expression($key,$value,$escape=false) 会原样拼接，需检查调用方是否传入用户输入。"
      },
      {
        "path": "var/Typecho/Request.php",
        "status": "reviewed",
        "note": "[布丁] __construct array_merge($_POST,$_GET)（GET 覆盖 POST）。"
      },
      {
        "path": "var/Typecho/Router.php",
        "status": "reviewed",
        "note": "[未命名 Agent] match() 用 routingTable 的正则 preg_match pathInfo；路由表来自 DB 序列化，用户不可控。\n[未命名 Agent] dispatch/match 用 routingTable regx 匹配；action 由 route 参数注入。"
      },
      {
        "path": "var/Typecho/Router/Parser.php",
        "status": "reviewed",
        "note": "[未命名 Agent] alpha 正则 ([_0-9a-zA-Z-]+) 含连字符，故 links-edit 可匹配 /action/[action:alpha]。"
      },
      {
        "path": "var/Typecho/Validate.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 通用校验类，规则由调用方定义，无自身注入。"
      },
      {
        "path": "var/Typecho/Widget.php",
        "status": "reviewed",
        "note": "[未命名 Agent] widget() 实例化时调用 execute()(221)，__call 走插件钩子；确认 Do.php 分发链路生效。"
      },
      {
        "path": "var/UI/Widget/Menu.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 占位无意义路径"
      },
      {
        "path": "var/Upgrade.php",
        "status": "reviewed",
        "note": "[小笼包] 升级迁移脚本集合，仅由 Widget_Upgrade(admin) 调用。行103-109 向 config.inc.php 追加时区语句，值来自 admin 配置。非直接可达，未发现可利用点。"
      },
      {
        "path": "var/Widget/Abstract/Comments.php",
        "status": "reviewed",
        "note": "[布丁] 评论抽象：filter/push/输出；author()/gravatar() 输出未转义（模板层），无写操作与鉴权缺失。"
      },
      {
        "path": "var/Widget/Abstract/Contents.php",
        "status": "reviewed",
        "note": "[未命名 Agent] getPageOffset() 第234行 “table.contents.{$column} \u003e {$offset}” 字符串拼接；调用方 Post($created 刷新为int)/Attachment(filter int) 安全；仅 Users/Edit 传入原始 request uid（admin-only）。第682行 unserialize 输入为 DB serialize 数据。"
      },
      {
        "path": "var/Widget/Abstract/Metas.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 标准 CRUD，insert/update/delete 用 rows()/where('?') 参数化，无注入。"
      },
      {
        "path": "var/Widget/Abstract/Options.php",
        "status": "reviewed",
        "note": "[小笼包] 选项表 CRUD 助手(insert/update/delete/size)，无鉴权逻辑，仅供上层调用。"
      },
      {
        "path": "var/Widget/Abstract/Users.php",
        "status": "reviewed",
        "note": "[布丁] 用户抽象：nameExists/mailExists/screenNameExists 参数化；getPageOffset 字符串拼接但调用方传常量。"
      },
      {
        "path": "var/Widget/Ajax.php",
        "status": "reviewed",
        "note": "[布丁] action 要求 isAjax；checkVersion pass('editor')、editorResize pass('contributor')，remoteCallback 仅回显 OK。\n[未命名 Agent] action() 无 security-\u003eprotect()（CSRF 缺失），各 handler pass('editor'/'subscriber'/'contributor')；影响面低（editorResize 写自身 editorSize 且 Options 只加载 user=0 行，实际未回显）。"
      },
      {
        "path": "var/Widget/Backup.php",
        "status": "reviewed",
        "note": "[未命名 Agent] action() 强制 pass('administrator')+protect()；导出/导入仅管理员；import 的 file 参数拼 __TYPECHO_BACKUP_DIR__（需管理员）。非越权。"
      },
      {
        "path": "var/Widget/Comments/Edit.php",
        "status": "reviewed",
        "note": "[小笼包] 评论编辑动作。action() 有 pass('contributor')+protect()；waiting/spam/approved/delete/getComment/editComment/replyComment 均先经 commentIsWriteable()(Abstract/Comments 中 editor 或 ownerId==uid)。未发现越权。"
      },
      {
        "path": "var/Widget/Contents/Attachment/Admin.php",
        "status": "reviewed",
        "note": "[布丁] 文件列表：execute 用 pass('editor',true) 过滤范围，参数化查询。"
      },
      {
        "path": "var/Widget/Contents/Attachment/Edit.php",
        "status": "reviewed",
        "note": "[未命名 Agent] updateAttachment() 第175行 title=原始 request name 未净化 -\u003e manage-medias.php:68 title() 原样输出 =\u003e 存储型 XSS（已提交 finding）。delete/clear 均经 isWriteable（editor 或作者本人）+protect，无 IDOR/CSRF。第178行 unserialize 输入来自 DB serialize 数据，不可控。"
      },
      {
        "path": "var/Widget/Contents/Attachment/Related.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 仅按 parentId 参数化查询附件，无洞。"
      },
      {
        "path": "var/Widget/Contents/Attachment/Unattached.php",
        "status": "reviewed",
        "note": "[小笼包] 未关联附件查询，按 authorId=当前 uid 过滤，只读，无越权。"
      },
      {
        "path": "var/Widget/Contents/Page/Edit.php",
        "status": "reviewed",
        "note": "[布丁] 页面编辑：execute pass('editor')，action protect()。受保护。"
      },
      {
        "path": "var/Widget/Contents/Post/Edit.php",
        "status": "reviewed",
        "note": "[未命名 Agent] execute() pass('contributor')，action() protect()；writePost 字段经 from()/getCreated，getCreated 第127-128行接收原始 request created 但 publish 后 push 刷新为 DB 值，未观测到注入；publish/save 有 editor 判定；Contents.php:569 expression 经 intval 安全。"
      },
      {
        "path": "var/Widget/Do.php",
        "status": "reviewed",
        "note": "[布丁] 动作分发：合并 _map 与 options-\u003eactionTable，无集中鉴权/CSRF，直接调用 widget-\u003eaction()（Links 漏洞根因）。\n[未命名 Agent] execute() 合并 _map 与 actionTable 后直接调用 action()，无统一鉴权/CSRF，插件动作须自检。\n[小笼包] 动作分发入口 execute()：action 来自 request，映射 _map 合并 options-\u003eactionTable(插件注册的动作)，命中且类实现 Widget_Interface_Do 即直接调用 action()，全程无任何集中鉴权/CSRF。是插件 Action 未授权问题的根源。"
      },
      {
        "path": "var/Widget/Feedback.php",
        "status": "reviewed",
        "note": "[布丁] 评论提交：comment() 调 security-\u003eprotect()（commentsAntiSpam 控制 enable）+严格 validator。\n[小笼包] 前台反馈提交。action() 按 request-\u003etype 仅分发 comment/trackback（in_array 白名单），comment() 有 security-\u003eprotect()、referer/间隔校验，表单字段经 Validate 校验后经查询构建器写入。未见越权/注入。"
      },
      {
        "path": "var/Widget/Init.php",
        "status": "reviewed",
        "note": "[布丁] 初始化：installed 为空时回写 installed=1，无外部可控参数。"
      },
      {
        "path": "var/Widget/Login.php",
        "status": "reviewed",
        "note": "[未命名 Agent] protect() 齐全；第75-76行 referer 经 response-\u003eredirect-\u003esafeUrl（仅校验 scheme http/https，不校验 host）=\u003e 登录后开放重定向（低）。"
      },
      {
        "path": "var/Widget/Logout.php",
        "status": "reviewed",
        "note": "[未命名 Agent] action() 仅调用 user-\u003elogout()，无鉴权需求；登出 CSRF 影响极小，不提交。"
      },
      {
        "path": "var/Widget/Menu.php",
        "status": "reviewed",
        "note": "[布丁] 菜单鉴权：行207 路径严格相等且菜单 query 参数齐全才在行258 pass();否则返回默认项。路径变体（大小写/PATH_INFO）理论可跳过 pass，但依赖服务器路径解析，未确认；audit-4 已作为 finding 提交。\n[未命名 Agent] getCurrentMenu 在 URL 与菜单项严格匹配时 do pass($access)(258)；URL 变体(validate=false)绕过点在 test 阶段属 audit-1/audit-4；未见未认证可直接访问后台页。\n[小笼包] 后台菜单与鉴权强制点。getCurrentMenu：$validate 需 URL path 与菜单项完全一致(行207)且 query 参数齐全(行210-215)、extending.php panel 一致(行218-223)才在行258调用 user-\u003epass($access)；否则跳过 pass。但各后台页面 widget 自身多会再调 pass，故需逐页确认是否存在仅靠 Menu 兜底的入口。未发现可直接利用点。"
      },
      {
        "path": "var/Widget/Metas/Category/Edit.php",
        "status": "reviewed",
        "note": "[布丁] 分类编辑：execute pass('editor')，action protect()。受保护。"
      },
      {
        "path": "var/Widget/Metas/Tag/Edit.php",
        "status": "reviewed",
        "note": "[未命名 Agent] execute pass('editor'), action protect()；全部 SQL 参数化，安全。"
      },
      {
        "path": "var/Widget/Options.php",
        "status": "reviewed",
        "note": "[未命名 Agent] execute() 无条件执行：unserialize theme:$theme(365)、plugins(390)、routingTable(401) 均来自 options 表，仅管理员可写，非攻击者可控；无独立漏洞。"
      },
      {
        "path": "var/Widget/Options/General.php",
        "status": "reviewed",
        "note": "[小笼包] 基本设置：action() 有 pass('administrator')+protect()。removeShell() 黑名单缺 phtml/php3/php7/phar 等(加固点，但仅管理员可设，非直接漏洞)。"
      },
      {
        "path": "var/Widget/Plugins/Config.php",
        "status": "reviewed",
        "note": "[布丁] 插件配置：execute pass('administrator')，表单提交到 /action/plugins-edit。"
      },
      {
        "path": "var/Widget/Plugins/Edit.php",
        "status": "reviewed",
        "note": "[未命名 Agent] action pass('administrator')+protect；plugin 名经 filter('slug')+portal；require_once 路径受 admin 限制。"
      },
      {
        "path": "var/Widget/Plugins/List.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 仅 glob 列目录与 parseInfo，无危险 sink。"
      },
      {
        "path": "var/Widget/Register.php",
        "status": "reviewed",
        "note": "[小笼包] 注册动作：protect()；已登录或 allowRegister 关闭则重定向。密码用随机生成的 generatedPassword 哈希，非用户输入。无越权。"
      },
      {
        "path": "var/Widget/Security.php",
        "status": "reviewed",
        "note": "[布丁] token=md5(secret[\u0026authCode\u0026uid])，protect() 依赖 referer，属常规设计。\n[未命名 Agent] protect() 用 _ 参数与 md5(secret\u0026authCode\u0026uid\u0026referer) 比较，secret 不可知，CSRF 防护有效；由此反证 Links 缺 protect() 的严重性。"
      },
      {
        "path": "var/Widget/Service.php",
        "status": "reviewed",
        "note": "[布丁] action 仅 do=ping -\u003e sendPingHandle() pass('contributor')。受保护。\n[未命名 Agent] sendPingHandle pass('contributor') 无 protect（CSRF）；$this-\u003erequest-\u003ecid 拼入 widget 参数字符串；pingback 处理因 scheme 判断恒 continue 影响有限。"
      },
      {
        "path": "var/Widget/Stat.php",
        "status": "reviewed",
        "note": "[未命名 Agent] 仅参数化 COUNT 统计查询，无注入/越权。"
      },
      {
        "path": "var/Widget/Themes/Edit.php",
        "status": "reviewed",
        "note": "[小笼包] 主题编辑：action() pass('administrator')+protect()。editThemeFile 由 request-\u003eedit(未过滤) 与 theme(slug) 拼路径，themeFile 的 trim(...,'./') 不阻止中段 ../ 穿越，属管理员任意文件写；但管理员已可改主题/插件文件，增量为低，未单独提交。"
      },
      {
        "path": "var/Widget/Themes/Files.php",
        "status": "reviewed",
        "note": "[布丁] 主题文件列表：execute pass('administrator')，theme/file 名严格正则+is_dir/file_exists。"
      },
      {
        "path": "var/Widget/Upgrade.php",
        "status": "reviewed",
        "note": "[未命名 Agent] action pass('administrator')+protect；upgrade() 的 $package 来自 get_class_methods('Upgrade')，非用户可控。"
      },
      {
        "path": "var/Widget/Upload.php",
        "status": "reviewed",
        "note": "[未命名 Agent] action() 有 pass('contributor',true)+isPost+protect()；文件名随机 crc32.ext，扩展名经 checkFileType/allowedAttachmentTypes 白名单，无路径穿越。未发现漏洞。"
      },
      {
        "path": "var/Widget/User.php",
        "status": "reviewed",
        "note": "[小笼包] 认证核心：hasLogin 用 uid+authCode 严格(===)校验；pass() 用组数字比较(administrator0 最高)。simpleLogin($uid) 仅凭 uid 登录且无任何调用方(全仓库无调用)，为死代码不可利用。pass() 中若用户组字符串不在 groups 映射，$this-\u003egroups[$this-\u003egroup] 为 null 且 null\u003c=任意非负值 为真，可能绕过；但用户组只能由管理员经 Users/Edit 修改，低权限无法自改。"
      },
      {
        "path": "var/Widget/Users/Admin.php",
        "status": "reviewed",
        "note": "[布丁] 后台用户列表：execute 无 pass（依赖 Menu），keywords 参数化 LIKE。\n[小笼包] 后台成员列表组件：execute()(行79-98) 无任何 pass 检查，直接列出全部用户——Menu 鉴权绕过后即未授权泄露用户信息的关键 sink。"
      },
      {
        "path": "var/Widget/Users/Author.php",
        "status": "reviewed",
        "note": "[未命名 Agent] execute 用 parameter-\u003euid 参数化查询，安全。"
      },
      {
        "path": "var/Widget/Users/Edit.php",
        "status": "reviewed",
        "note": "[未命名 Agent] execute()/action() 均 pass('administrator')+protect()，未授权不可达。"
      },
      {
        "path": "var/Widget/Users/Profile.php",
        "status": "reviewed",
        "note": "[小笼包] 个人资料动作：execute() pass('subscriber') 并强制 uid=当前用户；updateProfile 仅写 mail/screenName/url(不含 group)；updateOptions/updatePassword 均限定 uid=自己。updatePersonal 用 request-\u003eplugin 经 Typecho_Plugin::portal 解析插件文件，但 parseInfo 仅 file_get_contents+token 解析(不 include)，且随后校验插件已激活，未见 LFI。"
      },
      {
        "path": "var/Widget/XmlRpc.php",
        "status": "reviewed",
        "note": "[布丁] action 建 IXR_Server，各方法 checkAccess(用户名,密码,level) 校验；allowXmlRpc==0 时 404；无未授权调用。"
      }
    ],
    "unreviewed": []
  },
  "variables": [
    {
      "name": "link['name']/link['url']/link['image'] (Links 插件)",
      "path": "usr/plugins/Links/Action.php",
      "status": "suspicious",
      "note": "[布丁] 来源：Links_Action::insertLink()/updateLink() 从 request-\u003efrom('name','url','sort','image','description','user') 取出，直接 rows($link) 写入 typecho_links 表，无鉴权、无过滤、无转义。渲染端 usr/plugins/Links/manage-links.php 第53/54/58 行直接 echo $link['name']/$link['url']/$link['image'] 未转义，形成存储型 XSS。写入入口 /action/links-edit 由 Do.php 分发且无集中鉴权。"
    },
    {
      "name": "$input['name'] / $attachment['title'] (附件标题)",
      "path": "var/Widget/Contents/Attachment/Edit.php",
      "status": "suspicious",
      "note": "[未命名 Agent] updateAttachment() 第172行 `$input = $this-\u003erequest-\u003efrom('name','slug','description')`；第175行 `$attachment['title'] = $input['name']` 无任何净化/转义，直接写入 contents.title。对比核心上传路径 Widget_Upload::getSafeName() 会删除 \\\" \u003c \u003e，此编辑路径缺失该校验。"
    },
    {
      "name": "request-\u003eaction / actionTable",
      "path": "var/Widget/Do.php",
      "status": "suspicious",
      "note": "[未命名 Agent] request-\u003eaction 来自路由 /action/[action:alpha]（含-），Do.php:74 查 actionTable['links-edit']-\u003eLinks_Action 并直接调用 action()，无鉴权。用户可控，用于触发未授权插件动作。"
    },
    {
      "name": "$validate@var/Widget/Menu.php",
      "path": "var/Widget/Menu.php",
      "status": "suspicious",
      "note": "[小笼包] 行206 初始 true；行207 仅当菜单项 URL path 与当前请求 path(取自原始 REQUEST_URI，未规范化)完全相等才保持 true。为 false 时行256-258 的 user-\u003epass($access) 被跳过，且 _currentParent 保持默认 1。攻击者可用 PATH_INFO、/./、%2D 等路径变体使 validate=false，绕过后台页面鉴权。"
    }
  ],
  "flows": null,
  "project_note": {
    "note": "[布丁]\n【audit-1 布丁 审计笔记（入口可达性与身份权限）】\n\n项目：Typecho 老版 PHP CMS，后台目录 /haxwiki_admin/。\n\n【核心机制】\n- 前台入口：index.php -\u003e Widget_Init -\u003e Plugin::factory begin -\u003e Typecho_Router::dispatch -\u003e end。config.inc.php 定义路径/DB/httponly。\n- 动作分发：var/Widget/Do.php::execute() 合并 _map 与 unserialize(options-\u003eactionTable)，命中 class 且实现 Widget_Interface_Do 时直接 widget()-\u003eaction()，无任何集中鉴权/CSRF。插件通过 Helper::addAction 注册的 action 若不自查即未授权。\n- 后台页鉴权：haxwiki_admin/common.php 依赖 Widget_Menu::getCurrentMenu()；仅当请求 URL 路径与菜单项路径严格相等且菜单 query 参数都在当前 URL（validate）时，Menu.php:258 才 user-\u003epass($access)。否则 getCurrentMenu 返回默认项(控制台/subscriber)，不执行 pass。\n- CSRF：Security.token=md5(secret[\u0026authCode\u0026uid])，protect() 比对 request._ 与 getToken(referer)。\n- 权限组：administrator0/editor1/contributor2/subscriber3/visitor4。\n\n【已确认漏洞】\n- finding#1(high, CWE-862)：usr/plugins/Links/Action.php action() 无 pass/protect，游客 POST /action/links-edit?do=insert|update|delete|sort 直接写/删友情链接表；渲染端 manage-links.php:53/54/58 与 Plugin.php output_str():284-288 未转义，构成前台/后台存储型 XSS。\n\n【已排查无独立漏洞】\n- Actions 覆盖：Backup/Comments/Contents-*-Edit/Login/Register/Metas/Options-*/Plugins-Edit/Themes-Edit/Upgrade/Upload/Users-Edit/Users-Profile 均有 protect；Ajax(checkVersion/editorResize)/Service(sendPingHandle) 有 pass；Feedback 有 protect；XmlRpc 走 checkAccess(用户名,密码,level)。\n- 后台页 manage-users.php/profile.php/index.php 等自身无 pass，仅靠 Menu（设计依赖）。\n- var/Typecho/Request.php __construct array_merge($_POST,$_GET)；Db/Adapter/Mysql.php quoteValue 手写转义；IXR/Base64、IXR/Value 无 RCE。\n\n【待确认线索（未报）】\n- Menu.php:207 validate 依赖原始 REQUEST_URI；若请求路径与菜单项不完全一致（PATH_INFO/大小写，且服务器仍执行该 php），validate=false 跳过 pass -\u003e 低权限读管理员页。依赖 Web 服务器路径解析，本环境无法证实；audit-4 已作为 finding 提交。\n- extending.php 插件面板有 panelTable urlencode 白名单，简单 case/编码绕过已被阻断。\n\n【跨模块】audit-3 负责 Links/Smilies/Action、Do.php；我已完成入口/身份/权限角度复核并与 recon-4/audit-4 交叉验证。\n\n[未命名 Agent]\naudit-2 结论：已提交 finding id=1 (medium, CWE-79) 附件编辑标题存储型 XSS（Attachment/Edit.php:175 title=原始 request name 未净化 -\u003e manage-medias.php:68 title() 原样输出，管理员打开媒体库触发）。install.php:291 反序列化因 installed=1 守卫 404 不可达（诱饵）。Users/Edit getPageOffset 拼接 SQLi 仅 admin 可达（未报）。Ajax/Service 缺 protect(CSRF 低)。Login referer 开放重定向（低）。QiniuFile 上传未净化文件名（需插件激活，条件性未报）。IXR 无 XXE。我负责的 22 个文件已全部 reviewed。\n\n[未命名 Agent]\n【audit-3 危险sink与实际影响 审计笔记】项目：Typecho 老版 PHP CMS，后台目录 /haxwiki_admin/。核心入口/分发：index.php -\u003e Widget_Init -\u003e Router::dispatch；路由 /action/[action:alpha] -\u003e Widget_Do；Parser.php:60 alpha 正则含连字符。Typecho_Widget::widget() 实例化即 execute()(Widget.php:221)；Do.php:72-82 合并 _map 与 unserialize(options.actionTable) 后直接 action()，无统一鉴权/CSRF。已确认漏洞(Finding #1, high)：usr/plugins/Links/Action.php:83 action() 无 pass()/protect()，未认证可经 /action/links-edit?do=insert|update|delete|sort 增删改 links 表；Plugin.php:249-291 output_str() str_replace 未转义 -\u003e 存储型XSS。对照 Smilies_Action 有 protect()。已排除：install.php:291 反序列化(config 存在且 installed==1 时 :63-65 直接 404)；Upload 文件名随机+扩展名白名单；Db 层 #param 延迟转义+PDO prepare；Options.php unserialize 来自管理员可写 options 表；Backup 仅管理员。未闭环移交：Menu::getCurrentMenu(207/256-259) URL 变体可能跳过硬校验但未登录会重定向 welcome.php，需低权已登录账号验证(移交 audit-1/audit-4)；XmlRpc 方法级鉴权(audit-2)。\n\n[小笼包]\n【项目】Typecho 老版 PHP CMS，后台目录 haxwiki_admin，工作区 E:/网站数据备份/key08.com。\n【架构】index.php→Widget_Init→Router::dispatch；后台页面直接 include common.php(初始化)→header/menu.php→渲染各 Widget；动作经 /action/{action}→Widget_Do::execute（_map 合并 options-\u003eactionTable），命中且实现 Widget_Interface_Do 即直接调用 action()，无集中鉴权/CSRF。\n【鉴权】Widget_User::hasLogin 用 uid+authCode 严格(===)校验；pass() 用组数字(admin=0 最高)。simpleLogin($uid) 无调用方=死代码。后台 GET 页面主要由 Widget_Menu::getCurrentMenu 行258 的 pass() 兜底；写操作(/action)多由各 widget 的 action() 自查 pass+protect()。\n【已确认漏洞(我, audit-4)】finding#1 high：Menu.php 行207 $validate 依赖原始 REQUEST_URI 与菜单项 path 精确相等，可用 PATH_INFO/./%2D 变体使其为 false 跳过 pass，配合 Cookie __typecho_first_run=1 绕过 welcome 重定向，未授权访问仅靠 Menu 强制的后台页（如 manage-users.php→Widget_Users_Admin 无 pass，泄露全部用户）。\n【已确认漏洞(同伴)】audit-3：usr/plugins/Links/Action.php 无 pass/protect，游客可 POST /action/links-edit 增删改友情链接（对比 Smilies/Action.php 有 protect）。\n【加固点(未提交)】Options/General::removeShell 黑名单缺 phtml/php3/php7/phar 等(仅管理员可设)；Themes/Edit::editThemeFile 经 request-\u003eedit 未过滤 + themeFile trim(...,'./') 不阻中段 ../，管理员任意文件写(管理员本可改主题/插件文件，增量低)；Widget_User::pass 对非法组字符串 null\u003c=N 恒真的边界(用户组不可自改，未利用)。\n【注意】var/Typecho/Db/Query.php 延迟转义+prepare 单次替换安全；Mysql 适配器 quoteValue 用引号加倍(对 GBK 亦安全)。ColorHighlight/HyperDown 内容过滤已转义。\n【跨模块待办】其余后台页(Post/Admin、Comments/Admin、Plugins/List 等)亦仅靠 Menu 强制，可由 finding#1 波及。"
  }
}