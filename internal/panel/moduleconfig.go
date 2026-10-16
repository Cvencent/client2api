package panel

// This file is the panel's plain-language index of the settings each
// module reads out of `clients.<name>` in the config file.
//
// Background: `clients` is a map of raw JSON handed to each module, so the
// panel used to render it as one unlabelled JSON textarea per module. That is
// a faithful view of the file and a useless one for an operator: nothing says
// what `tls_profile` is, which values it accepts, or what "unset" means.
//
// This file is the missing index. It is deliberately a curated table rather
// than a dump of every struct tag: a module's internal base URLs, OAuth
// endpoints and per-request timeouts are tuning knobs, not settings. Whatever
// is not listed here still reaches the operator through the "其它键" JSON box
// on the same card, so nothing becomes unreachable -- it just stops being the
// only thing on screen.

// moduleOptionKind selects the control the panel renders and how it turns the
// operator's text back into JSON.
type moduleOptionKind string

const (
	// optText is a free-text string.
	optText moduleOptionKind = "text"
	// optBool renders as 默认 / 开 / 关. The empty choice deletes the key, which
	// is how a module falls back to its own default.
	optBool moduleOptionKind = "bool"
	// optChoice renders as a dropdown of the declared values.
	optChoice moduleOptionKind = "choice"
	// optNumber is a JSON number.
	optNumber moduleOptionKind = "number"
	// optList is a comma-separated list of strings.
	optList moduleOptionKind = "list"
)

// moduleOptionChoice is one entry of a dropdown.
type moduleOptionChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// moduleOption is one labelled setting.
type moduleOption struct {
	// Key is the exact JSON key under `clients.<module>`. It has to match the
	// module's own struct tag, which moduleconfig_test.go checks against the
	// example config.
	Key string `json:"key"`
	// Label is what the operator reads. Chinese, like the rest of the panel.
	Label string           `json:"label"`
	Kind  moduleOptionKind `json:"kind"`
	// Help says what the setting does and, where it matters, what an empty
	// value means. It is shown under the control.
	Help string `json:"help,omitempty"`
	// Choices is only meaningful for optChoice.
	Choices []moduleOptionChoice `json:"choices,omitempty"`
}

// tlsProfileChoices mirrors internal/fingerprint's supported profiles. A
// module that names one makes its outbound TLS handshake look like that
// client; the empty value leaves Go's own handshake in place.
var tlsProfileChoices = []moduleOptionChoice{
	{Value: "", Label: "默认（不伪装，用 Go 自己的握手）"},
	{Value: "golang", Label: "golang（与默认同样的握手，显式写出）"},
	{Value: "node", Label: "node（厂商 CLI 自带的 Node.js）"},
	{Value: "trae", Label: "trae（Trae 桌面端）"},
	{Value: "chrome", Label: "chrome（跟随 uTLS 最新版）"},
	{Value: "chrome-133", Label: "chrome-133"},
	{Value: "chrome-131", Label: "chrome-131"},
	{Value: "chrome-120", Label: "chrome-120"},
	{Value: "chrome-106", Label: "chrome-106"},
	{Value: "firefox", Label: "firefox"},
	{Value: "firefox-120", Label: "firefox-120"},
	{Value: "safari", Label: "safari"},
	{Value: "edge", Label: "edge"},
	{Value: "ios", Label: "ios"},
	{Value: "android", Label: "android"},
	{Value: "randomized", Label: "randomized（随机化）"},
}

// tlsProtocolChoices mirrors internal/fingerprint.Protocol.
var tlsProtocolChoices = []moduleOptionChoice{
	{Value: "", Label: "默认（跟随指纹：先试 h2，失败退回 http/1.1）"},
	{Value: "http/1.1", Label: "http/1.1（只走 HTTP/1.1）"},
	{Value: "h2", Label: "h2（必须 HTTP/2，协商不到就报错）"},
}

const (
	tlsProfileHelp   = "出站 TLS 握手的指纹。厂商按 JA3/JA4 识别客户端时从这里选对应的那个；不认识自己在做什么就留空。"
	tlsProtocolHelp  = "协商的 HTTP 版本。默认跟随指纹；只在确认上游不开 h2 时才强制 http/1.1。"
	freeOnlyHelp     = "只允许厂商标价为零的免费模型，付费模型在本地就被拒掉，不会真的发请求。"
	autoDiscoverHelp = "启动时扫描本机已登录的桌面客户端，把找到的凭据自动加进账号池。"
)

// moduleOptions is the curated index. A module that is absent here (or that is
// not registered at all) renders with an empty form and only the "其它键" box.
var moduleOptions = map[string][]moduleOption{
	"autoclaw": {
		{Key: "auto_discover", Label: "自动发现本机凭据", Kind: optBool, Help: autoDiscoverHelp},
		{Key: "credential_paths", Label: "凭据文件路径", Kind: optList, Help: "额外扫描的账号 JSON 路径，多个用逗号分隔。"},
		{Key: "base_url", Label: "接口地址", Kind: optText, Help: "上游 API 根地址。留空用模块内置的官方地址。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
		{Key: "client_version", Label: "客户端版本号", Kind: optText, Help: "随请求上报的客户端版本。留空用模块内置值。"},
		{Key: "sms_token", Label: "接码平台 Token", Kind: optText, Help: "用于自动接收短信验证码。留空表示不启用接码。"},
		{Key: "sms_keyword", Label: "接码关键词", Kind: optText, Help: "从短信里匹配的关键词，例如厂商名。留空用模块内置值（智谱华章）。"},
		{Key: "sms_base", Label: "接码平台地址", Kind: optText, Help: "接码服务的 API 根地址。留空用模块内置值。"},
		{Key: "sms_provinces", Label: "接码号段", Kind: optList, Help: "优先取哪些省份的号码，多个用逗号分隔。"},
		{Key: "sms_card_type", Label: "接码卡类型", Kind: optText, Help: "传给接码平台的卡类型参数。留空用模块内置值。"},
		{Key: "auto_login_timeout_seconds", Label: "自动登录超时（秒）", Kind: optNumber, Help: "一整轮自动登录最多等多久。"},
		{Key: "sms_polls", Label: "接码轮询次数", Kind: optNumber, Help: "最多查几次短信。"},
		{Key: "sms_interval_seconds", Label: "接码轮询间隔（秒）", Kind: optNumber, Help: "两次查短信之间等多久。"},
		{Key: "dup_retries", Label: "重复号码重试次数", Kind: optNumber, Help: "拿到已用过的号码时最多重试几次。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"cline": {
		{Key: "free_only", Label: "只用免费模型", Kind: optBool, Help: freeOnlyHelp},
		{Key: "reasoning_effort", Label: "思考档位默认值", Kind: optText, Help: "请求没带思考档位时用哪个，例如 low / medium / high。留空用模块内置值。"},
		{Key: "max_concurrency", Label: "单账号并发上限", Kind: optNumber, Help: "同一账号同时进行的请求数；留空用模块内置值。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"codearts": {
		{Key: "models", Label: "模型清单", Kind: optList, Help: "覆盖模块自带的模型列表，多个用逗号分隔。"},
		{Key: "reasoning_effort", Label: "思考档位默认值", Kind: optText, Help: "例如 low / medium / high。留空用模块内置值。"},
		{Key: "max_tokens", Label: "默认最大输出 tokens", Kind: optNumber, Help: "请求没写 max_tokens 时用它。留空用模块内置值。"},
		{Key: "login", Label: "允许面板登录", Kind: optBool, Help: "打开后账号页会多出浏览器登录入口。"},
	},
	"doubao": {
		{Key: "web_base", Label: "网页版地址", Kind: optText, Help: "上游根地址。留空用模块内置值。"},
		{Key: "chat_bot_id", Label: "对话 Bot ID", Kind: optText, Help: "调用哪个内置对话机器人。留空用模块内置值。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "browser_path", Label: "浏览器路径", Kind: optText, Help: "面板登录时启动的浏览器可执行文件；留空自动查找。"},
		{Key: "browser_headless", Label: "浏览器无头模式", Kind: optBool, Help: "打开后登录浏览器不显示窗口。"},
		{Key: "client_user_data_dir", Label: "浏览器用户目录", Kind: optText, Help: "复用哪个浏览器配置目录。留空用临时目录。"},
	},
	"doubaoime": {
		{Key: "ffmpeg_path", Label: "ffmpeg 路径", Kind: optText, Help: "语音转写需要 ffmpeg 把录音转成 Opus。留空表示直接用 PATH 里的 ffmpeg（默认 ffmpeg）。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
	},
	"gemini": {
		{Key: "client_id", Label: "Google OAuth Client ID", Kind: optText, Help: "只在自建 OAuth 客户端时填写；留空用模块内置的 Cloud Code 客户端。"},
		{Key: "client_secret", Label: "Google OAuth Client Secret", Kind: optText, Help: "与 Client ID 配套；留空用模块内置值。"},
		{Key: "project", Label: "Cloud Code 项目 ID", Kind: optText, Help: "留空时先探测账号绑定的项目，再退回默认项目。"},
		{Key: "login_port", Label: "登录回调端口", Kind: optNumber, Help: "面板登录时本地监听的端口。留空由系统分配。"},
	},
	"kilocode": {
		{Key: "free_only", Label: "只用免费模型", Kind: optBool, Help: freeOnlyHelp},
		{Key: "anonymous", Label: "匿名模式", Kind: optBool, Help: "打开后不携带账号身份，走厂商的匿名额度。"},
		{Key: "mode", Label: "模式", Kind: optText, Help: "模块自定义的运行模式。留空用模块内置值。"},
		{Key: "version", Label: "客户端版本号", Kind: optText, Help: "随请求上报的版本。留空用模块内置值。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
		{Key: "extra_models", Label: "额外模型", Kind: optList, Help: "在厂商模型表之外补进来的模型名，多个用逗号分隔。"},
		{Key: "max_in_flight", Label: "并发上限", Kind: optNumber, Help: "同时在飞的请求数上限；0 表示不限制。"},
		{Key: "test_model", Label: "测试用模型", Kind: optText, Help: "账号页「测试」按钮用的模型。留空用模块内置值。"},
	},
	"kimi": {
		{Key: "binary", Label: "Kimi CLI 路径", Kind: optText, Help: "要调用的 CLI 可执行文件；留空用模块内置路径。"},
		{Key: "default_model", Label: "默认模型", Kind: optText, Help: "请求没指定模型时用哪个。留空用模块内置值。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "permission_mode", Label: "权限模式", Kind: optText, Help: "传给 CLI 的权限模式。留空用模块内置值。"},
		{Key: "skills_dir", Label: "技能目录", Kind: optText, Help: "CLI 加载技能的位置。留空用模块内置值。"},
		{Key: "cwd", Label: "工作目录", Kind: optText, Help: "CLI 进程的工作目录。留空用模块内置值。"},
		{Key: "max_concurrency", Label: "并发上限", Kind: optNumber, Help: "同时运行的 CLI 进程数。"},
		{Key: "timeout_seconds", Label: "单次超时（秒）", Kind: optNumber, Help: "一次调用最多等多久。"},
		{Key: "media_dir", Label: "媒体目录", Kind: optText, Help: "图片等附件的落地目录。留空用模块内置值。"},
		{Key: "allow_image_download", Label: "允许下载图片", Kind: optBool, Help: "打开后会把上游返回的图片下载到媒体目录。"},
		{Key: "login_mode", Label: "登录方式", Kind: optText, Help: "面板登录时走哪种流程。留空用模块内置值。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"lobsterai": {
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
		{Key: "include_usage", Label: "返回用量统计", Kind: optBool, Help: "打开后流式响应里会带上 token 用量。"},
		{Key: "extra_models", Label: "额外模型", Kind: optList, Help: "在厂商模型表之外补进来的模型名，多个用逗号分隔。"},
		{Key: "max_in_flight", Label: "并发上限", Kind: optNumber, Help: "同时在飞的请求数上限；0 表示不限制。"},
		{Key: "login_port", Label: "登录回调端口", Kind: optNumber, Help: "面板登录时本地监听的端口。留空由系统分配。"},
	},
	"loomy": {
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
		{Key: "sms_token", Label: "接码平台 Token", Kind: optText, Help: "用于自动接收短信验证码。留空表示不启用接码。"},
		{Key: "sms_keyword", Label: "接码关键词", Kind: optText, Help: "从短信里匹配的关键词，例如厂商名。留空用模块内置值。"},
		{Key: "sms_base", Label: "接码平台地址", Kind: optText, Help: "接码服务的 API 根地址。留空用模块内置值。"},
		{Key: "sms_provinces", Label: "接码号段", Kind: optList, Help: "优先取哪些省份的号码，多个用逗号分隔。"},
		{Key: "auto_login_timeout_seconds", Label: "自动登录超时（秒）", Kind: optNumber, Help: "一整轮自动登录最多等多久。"},
		{Key: "sms_polls", Label: "接码轮询次数", Kind: optNumber, Help: "最多查几次短信。"},
		{Key: "sms_interval_seconds", Label: "接码轮询间隔（秒）", Kind: optNumber, Help: "两次查短信之间等多久。"},
		{Key: "dup_retries", Label: "重复号码重试次数", Kind: optNumber, Help: "拿到已用过的号码时最多重试几次。"},
	},
	"minimaxcode": {
		{Key: "auto_discover", Label: "自动发现本机凭据", Kind: optBool, Help: autoDiscoverHelp},
		{Key: "auth_dir", Label: "凭据目录", Kind: optText, Help: "从哪个目录读取桌面客户端登录态。留空用模块内置路径。"},
		{Key: "config_yaml", Label: "客户端配置文件", Kind: optText, Help: "要读取的客户端 YAML 配置路径。留空用模块内置路径。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "max_tokens_default", Label: "默认最大输出 tokens", Kind: optNumber, Help: "请求没写 max_tokens 时用它。"},
		{Key: "max_account_attempts", Label: "单请求换号次数", Kind: optNumber, Help: "一个账号失败后最多再试几个账号。"},
		{Key: "cooldown_seconds", Label: "失败冷却（秒）", Kind: optNumber, Help: "账号失败后停用多久。"},
		{Key: "timeout_seconds", Label: "单次超时（秒）", Kind: optNumber, Help: "一次调用最多等多久。"},
		{Key: "timezone", Label: "时区", Kind: optText, Help: "随请求上报的时区，例如 Asia/Shanghai。留空用模块内置值。"},
		{Key: "refresh_enabled", Label: "自动刷新登录态", Kind: optBool, Help: "打开后会在过期前自动刷新 token。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"opencode": {
		{Key: "free_only", Label: "只用免费模型", Kind: optBool, Help: freeOnlyHelp},
		{Key: "default_realm", Label: "默认 Realm", Kind: optText, Help: "登录默认走哪个区，例如 cn / global。留空由模块自己挑。"},
		{Key: "test_model", Label: "测试用模型", Kind: optText, Help: "账号页「测试」按钮用的模型。留空用模块内置值。"},
		{Key: "include_usage", Label: "返回用量统计", Kind: optBool, Help: "打开后流式响应里会带上 token 用量。"},
		{Key: "extra_models", Label: "额外模型", Kind: optList, Help: "在厂商模型表之外补进来的模型名，多个用逗号分隔。"},
		{Key: "disable_auth_json_discovery", Label: "禁止扫描本机 auth.json", Kind: optBool, Help: "打开后不再从本机 auth.json 里发现凭据。"},
		{Key: "max_in_flight", Label: "并发上限", Kind: optNumber, Help: "同时在飞的请求数上限；0 表示不限制。"},
	},
	"openrouter": {
		{Key: "free_only", Label: "只用免费模型", Kind: optBool, Help: freeOnlyHelp},
		{Key: "http_referer", Label: "HTTP Referer", Kind: optText, Help: "OpenRouter 用来做排行归因，随便填个站点地址即可。"},
		{Key: "x_title", Label: "X-Title", Kind: optText, Help: "同上，OpenRouter 面板里显示的应用名。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
		{Key: "extra_models", Label: "额外模型", Kind: optList, Help: "在厂商模型表之外补进来的模型名，多个用逗号分隔。"},
		{Key: "max_in_flight", Label: "并发上限", Kind: optNumber, Help: "同时在飞的请求数上限；0 表示不限制。"},
		{Key: "test_model", Label: "测试用模型", Kind: optText, Help: "账号页「测试」按钮用的模型。留空用模块内置值。"},
	},
	"qoder": {
		{Key: "chat_model", Label: "对话模型", Kind: optText, Help: "默认用哪个模型对话。留空用模块内置值。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "browser_path", Label: "浏览器路径", Kind: optText, Help: "自动登录时启动的浏览器；留空自动查找。"},
		{Key: "browser_headless", Label: "浏览器无头模式", Kind: optBool, Help: "打开后登录浏览器不显示窗口。"},
		{Key: "sms_token", Label: "接码平台 Token", Kind: optText, Help: "用于自动接收短信验证码。留空表示不启用接码。"},
		{Key: "sms_keyword", Label: "接码关键词", Kind: optText, Help: "从短信里匹配的关键词。留空用模块内置值。"},
		{Key: "sms_base", Label: "接码平台地址", Kind: optText, Help: "接码服务的 API 根地址。留空用模块内置值。"},
		{Key: "sms_provinces", Label: "接码号段", Kind: optList, Help: "优先取哪些省份的号码，多个用逗号分隔。"},
		{Key: "sms_card_type", Label: "接码卡类型", Kind: optText, Help: "传给接码平台的卡类型参数。留空用模块内置值。"},
		{Key: "auto_login_timeout_seconds", Label: "自动登录超时（秒）", Kind: optNumber, Help: "一整轮自动登录最多等多久。"},
		{Key: "sms_polls", Label: "接码轮询次数", Kind: optNumber, Help: "最多查几次短信。"},
		{Key: "sms_interval_seconds", Label: "接码轮询间隔（秒）", Kind: optNumber, Help: "两次查短信之间等多久。"},
		{Key: "dup_retries", Label: "重复号码重试次数", Kind: optNumber, Help: "拿到已用过的号码时最多重试几次。"},
	},
	"qwenwork": {
		{Key: "base_url", Label: "接口地址", Kind: optText, Help: "上游 API 根地址。留空用模块内置的官方地址。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
		{Key: "max_tokens", Label: "默认最大输出 tokens", Kind: optNumber, Help: "请求没写 max_tokens 时用它。留空用模块内置值。"},
		{Key: "login", Label: "允许面板登录", Kind: optBool, Help: "打开后账号页会多出浏览器登录入口。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"raccoon": {
		{Key: "base_url", Label: "接口地址", Kind: optText, Help: "上游 API 根地址。留空用模块内置的官方地址。"},
		{Key: "user_agent", Label: "User-Agent", Kind: optText, Help: "出站请求的 UA。留空用模块内置值。"},
		{Key: "client_platform", Label: "客户端平台标识", Kind: optText, Help: "随请求上报的平台，例如 desktop-windows。留空用模块内置值。"},
		{Key: "client_version", Label: "客户端版本号", Kind: optText, Help: "随请求上报的版本。留空用模块内置值。"},
		{Key: "credential_paths", Label: "凭据文件路径", Kind: optList, Help: "额外扫描的账号文件路径，多个用逗号分隔。"},
		{Key: "browser_path", Label: "浏览器路径", Kind: optText, Help: "自动登录时启动的浏览器；留空自动查找。"},
		{Key: "browser_headless", Label: "浏览器无头模式", Kind: optBool, Help: "打开后优先无窗口验证；阿里云拒绝时会自动用可见窗口重试一次。"},
		{Key: "sms_token", Label: "接码平台 Token", Kind: optText, Help: "用于自动接收短信验证码。留空表示不启用接码。"},
		{Key: "sms_keyword", Label: "接码关键词", Kind: optText, Help: "从短信里匹配的关键词。留空用模块内置值（商汤科技）。"},
		{Key: "sms_base", Label: "接码平台地址", Kind: optText, Help: "接码服务的 API 根地址。留空用模块内置值。"},
		{Key: "sms_provinces", Label: "接码号段", Kind: optList, Help: "优先取哪些省份的号码，多个用逗号分隔。"},
		{Key: "sms_card_type", Label: "接码卡类型", Kind: optText, Help: "传给接码平台的卡类型参数。留空用模块内置值。"},
		{Key: "auto_login_timeout_seconds", Label: "自动登录超时（秒）", Kind: optNumber, Help: "一整轮自动登录最多等多久。"},
		{Key: "sms_polls", Label: "接码轮询次数", Kind: optNumber, Help: "最多查几次短信。"},
		{Key: "sms_interval_seconds", Label: "接码轮询间隔（秒）", Kind: optNumber, Help: "两次查短信之间等多久。"},
		{Key: "dup_retries", Label: "重复号码重试次数", Kind: optNumber, Help: "拿到已用过的号码时最多重试几次。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"tabbit": {
		{Key: "manage", Label: "由网关管理进程", Kind: optBool, Help: "打开后网关自己拉起并守护 tabbit 进程，而不是连一个已经在跑的服务。"},
		{Key: "command", Label: "启动命令", Kind: optText, Help: "受管模式下要执行的程序路径。"},
		{Key: "args", Label: "启动参数", Kind: optList, Help: "传给启动命令的参数，多个用逗号分隔。"},
		{Key: "workdir", Label: "工作目录", Kind: optText, Help: "受管进程的工作目录。"},
		{Key: "transport", Label: "通信方式", Kind: optText, Help: "例如 http / cli。留空用模块内置值。"},
		{Key: "web_base_url", Label: "网页版地址", Kind: optText, Help: "走网页接口时的根地址。留空用模块内置值。"},
		{Key: "web_token", Label: "网页版令牌", Kind: optText, Help: "走网页接口时的凭据。留空表示不用网页接口。"},
		{Key: "tabbit_cli", Label: "tabbit CLI 路径", Kind: optText, Help: "走 CLI 时要执行的程序。留空用模块内置值。"},
		{Key: "model_prefix", Label: "模型名前缀", Kind: optText, Help: "列出的模型名统一加这个前缀。留空不加。"},
		{Key: "extra_models", Label: "额外模型", Kind: optList, Help: "额外补进来的模型名，多个用逗号分隔。"},
		{Key: "include_usage", Label: "返回用量统计", Kind: optBool, Help: "打开后流式响应里会带上 token 用量。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"trae": {
		{Key: "storage_path", Label: "客户端存储路径", Kind: optText, Help: "从哪里读取已登录的 Trae 状态。留空用模块内置路径。"},
		{Key: "storage_paths", Label: "额外存储路径", Kind: optList, Help: "多个客户端安装位置的补充路径，多个用逗号分隔。"},
		{Key: "auto_discover", Label: "自动发现本机凭据", Kind: optBool, Help: autoDiscoverHelp},
		{Key: "login_enabled", Label: "允许面板登录", Kind: optBool, Help: "打开后账号页会多出登录入口。"},
		{Key: "model", Label: "默认模型", Kind: optText, Help: "请求没指定模型时用哪个。留空用模块内置值。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "max_tokens", Label: "默认最大输出 tokens", Kind: optNumber, Help: "请求没写 max_tokens 时用它。"},
		{Key: "self_renew", Label: "自助续期", Kind: optBool, Help: "打开后凭据快过期时由模块自己续，而不是只标记失效。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
	"workbuddy": {
		{Key: "accounts_dir", Label: "账号目录", Kind: optText, Help: "WorkBuddy 凭据 JSON 所在目录；这里的账号不会进备份包，导入导出时要单独处理。"},
		{Key: "global_enabled", Label: "启用国际版", Kind: optBool, Help: "打开后除国内站外还加载国际站。"},
		{Key: "passthrough_ip", Label: "透传真实 IP", Kind: optBool, Help: "打开后把调用方的 IP 原样带给上游。"},
		{Key: "sanitize_fingerprints", Label: "出站指纹清洗（模块开关）", Kind: optBool, Help: "模块自己的开关，重启后生效。"},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "browser_path", Label: "浏览器路径", Kind: optText, Help: "自动登录时启动的浏览器；留空自动查找。"},
		{Key: "browser_headless", Label: "浏览器无头模式", Kind: optBool, Help: "打开后登录浏览器不显示窗口。"},
		{Key: "client_version", Label: "客户端版本号", Kind: optText, Help: "随请求上报的版本。留空用模块内置值。"},
		{Key: "cli_version", Label: "CLI 版本号", Kind: optText, Help: "随 CLI 请求上报的版本。留空用模块内置值。"},
		{Key: "login_realm", Label: "登录站点", Kind: optText, Help: "登录走国内站还是国际站。留空由模块自己挑。"},
		{Key: "sms_token", Label: "接码平台 Token", Kind: optText, Help: "用于自动接收短信验证码。留空表示不启用接码。"},
		{Key: "sms_keyword", Label: "接码关键词", Kind: optText, Help: "从短信里匹配的关键词。留空用模块内置值。"},
		{Key: "sms_base", Label: "接码平台地址", Kind: optText, Help: "接码服务的 API 根地址。留空用模块内置值。"},
		{Key: "sms_provinces", Label: "接码号段", Kind: optList, Help: "优先取哪些省份的号码，多个用逗号分隔。"},
	},
	"zcode": {
		{Key: "auto_discover", Label: "自动发现本机凭据", Kind: optBool, Help: autoDiscoverHelp},
		{Key: "models", Label: "模型清单", Kind: optList, Help: "对外提供的模型名，多个用逗号分隔。"},
		{Key: "max_tokens_default", Label: "默认最大输出 tokens", Kind: optNumber, Help: "请求没写 max_tokens 时用它。"},
		{Key: "max_account_attempts", Label: "单请求换号次数", Kind: optNumber, Help: "一个账号失败后最多再试几个账号。"},
		{Key: "cooldown_seconds", Label: "失败冷却（秒）", Kind: optNumber, Help: "账号失败后停用多久。"},
		{Key: "timeout_seconds", Label: "单次超时（秒）", Kind: optNumber, Help: "一次调用最多等多久。"},
		{Key: "inject_system_blocks", Label: "注入系统提示块", Kind: optBool, Help: "打开后按厂商客户端的样子补系统提示，伪装得更像官方客户端。"},
		{Key: "inject_cache_control", Label: "注入 cache_control", Kind: optBool, Help: "打开后在请求里补缓存标记，命中厂商的提示缓存。"},
		{Key: "captcha_browser", Label: "用内置浏览器过人机验证", Kind: optBool, Help: "签到遇到人机验证时，由网关自己拉起浏览器求解。"},
		{Key: "captcha_browser_path", Label: "验证用浏览器路径", Kind: optText, Help: "内置浏览器求解时用哪个可执行文件；留空自动查找。"},
		{Key: "captcha_command", Label: "外部验证命令", Kind: optText, Help: "自己写脚本求解时执行的命令；和上面的内置浏览器二选一。"},
		{Key: "captcha_args", Label: "外部验证命令参数", Kind: optList, Help: "传给外部验证命令的参数，多个用逗号分隔。"},
		{Key: "captcha_region", Label: "人机验证区域", Kind: optText, Help: "验证码服务的地域参数。留空用模块内置值。"},
		{Key: "captcha_pool", Label: "验证码预取池", Kind: optBool, Help: "打开后后台预取一次性验证码参数，请求时取用并立即补充；会更快，但也会更快消耗验证码设备额度。"},
		{Key: "captcha_pool_ttl_seconds", Label: "预取池有效期（秒）", Kind: optNumber, Help: "预取参数最多等待多久；过期就丢弃并重新预取。留空用默认 30 秒。"},
		{Key: "tls_profile", Label: "TLS 指纹", Kind: optChoice, Help: tlsProfileHelp, Choices: tlsProfileChoices},
		{Key: "tls_protocol", Label: "HTTP 版本", Kind: optChoice, Help: tlsProtocolHelp, Choices: tlsProtocolChoices},
	},
}

// moduleConfig returns the option list for one module, nil when the panel has
// nothing curated for it.
func moduleConfig(name string) []moduleOption {
	return moduleOptions[name]
}
