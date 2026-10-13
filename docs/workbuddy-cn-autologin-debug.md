# WorkBuddy CN auto-login debug notes

Date: 2026-10-13

## Symptom

CN 国内版“自动添加账号”在填完验证码、点“手机号登录”后，浏览器侧始终停在授权页，
模块轮询到超时失败。失败日志原来只有心跳和一句 `失败：超时`，看不出是哪一步断的。

## Decisive evidence (real CN number, 2026-10-13)

现在超时日志会打出真实的接口轨迹，一次真实验证的输出是：

    授权接口轨迹：POST /v1/auth/sms/code/send -> 200 | POST /v1/auth/sms/code/verify -> 200 | GET /v1/auth/accounts -> 200 accounts=1 next=login
    账号选择诊断：{"path":"/login/undefined","items":0,"oneid":0,...,"oneidState":"{\"sent\":false,...}"}

关键事实：send/verify/accounts 三步都到了厂商（HTTP 200），且 `/v1/auth/accounts`
返回了 1 个账号；但**下一步 `/v1/auth/select_account` 根本没有发出**，
隐藏 `#phone-iframe` 里的 OneID 组件仍停在手机号表单（`sent:false`）。

注意 HTTP 200 不等于业务成功：`/v1/auth/sms/code/verify` 可能返回体里带
`errCode`。所以卡点有两种可能：verify 业务失败（组件 fail，不进账号步骤），
或 verify 成功但 OneID 组件没有把事件上报给外层 flow。要区分两者，需要
verify 响应里的 `err=` / `extract=` 字段（追踪已记录，待一次不被限流的真实
验证采样）。

## Ground truth（从当前线上 SPA/vendor bundle 源码读出来，不是猜的）

- OneID 手机登录组件（`oneid_phone_login`，vendor bundle 约 102550-102660）：
  `handleSendCode` -> `/v1/auth/sms/code/send`（成功后 `isCodeSent=true`、记
  `sentMobile`）；`handleLogin` 校验表单与 `isCodeSent` 后 `verifySmsCode` ->
  `/v1/auth/sms/code/verify`；验证通过后 `success.emit(mo)`；
  外层 `handleLoginSuccess` 收到后 `success.emit({...})`。
- SPA 自己的 telemetry observer（`oneid-request-observer`）把
  `/v1/auth/sms/code/send|verify`、`/v1/auth/accounts`、`/v1/auth/select_account`
  映射成 `web.login.*` 事件，且只在 `flow.flow === "oneid_phone_component"`
  时上报——说明正常路径里：手机组件事件 -> 外层 flow 推进到账号步骤。
- `checkLoginDisabled` 只看手机号长度和验证码 6 位，“确认按钮可点”不代表验证码
  已被接受；真实证据里确认按钮可点但没发 verify 请求时，页面提示“请获取并输入验证码”。

## Why the old traces were empty

旧的 CN 请求追踪只 patch 了顶层 `window`，而 OneID 的 send/verify 是在隐藏
`#phone-iframe` 的窗口里发出的，所以一条都记不到。现在的实现：

- 顶层和每个同源 `iframe/frame` 都 patch fetch/XHR；
- 每次巡检重装一次（页面导航会换掉 window 对象）；
- 追踪镜像进 `sessionStorage`，页面跳转到 `/login/undefined` 也不会丢。

## Fixes shipped this round

- 删掉 `openPhoneLogin` 里的人工 `load` 重放，以及账号选择前的重放块与
  `cnONEIDSyncSettle`（人工合成 `load` 不是供应商原流程，且会覆盖 React state）。
- 提交选择器收紧为可点形态（排除 disabled/aria-disabled 占位）。
- CN 请求追踪：顶层 + 所有同源 frame、每 tick 重装、镜像 sessionStorage；
  过滤 `/console/`、`/v2/plugin/`、`/v1/auth/`、`/auth/`；只记
  method + path + HTTP status + 少量非身份字段（code / errCode / accounts 数量 /
  next_step / loginUrl 是否存在），绝不记 body、header、token、query。
- `confirmAutoLogin` 每次轨迹变化就打印 `授权接口轨迹`，超时前补一次快照。
- 诊断提前到第 15s，并多报 `phone` 输入框数量、顶层可见文案、以及隐藏
  iframe 的 `sent/policy/loginUrl` 状态。

## Remaining root cause to fix next (localized)

真实轨迹证据：`send 200`、`verify 200`、`accounts 200 accounts=1 next=login`，但
**没有 `/v1/auth/select_account`**，隐藏 `#phone-iframe` 里 OneID 组件仍停在手机号页
（`sent:false`，`__ONEID__.loginUrl` 存在）。

源码定位（vendor bundle）：

- OneID 手机组件的 `handleLogin` 在 `verifySmsCode` 返回 200 且 `mo.errCode` 为空时
  才会 `success.emit(mo)`；若返回体带 `errCode`，它会 `fail.emit` 或只在表单上弹错，
  不会进入账号步骤。**所以下一步要确认 verify 响应有没有 `errCode`**：日志里
  `POST /v1/auth/sms/code/verify -> 200` 目前只记了 HTTP 状态，没记 body 字段；
  patch 里其实已经加了 `err=` / `extract=` 字段，只是最近几次真实验证都被短信平台
  限流（“还没收到短信 12 次”）没走到 verify，所以尚未采到。
- vendor `handleLoginSuccess` 需要 `personal_extract_result.result === "success"`
  （或 `"safeVerify"`+`safe_verify_info`）才会 `success.emit({...})` 继续。若
  `result` 是别的值，流程也不推进——这就是 `extract=` 字段要抓的东西。
- 拿到 `err=` / `extract=` 后即可定位：是 verify 业务失败（手机号/风控/该号已被占用），
  还是 verify 成功但抽取结果非 success（需要额外校验/换号）。

定位清楚后再决定：换号、或按 `personal_extract_result` 分支处理、或调整外层驱动，
然后只做一次真实验证。不要靠反复试号。

## Do not repeat

1. 人工 dispatch iframe `load`（不是原流程，会覆盖 React state）。
2. `login-return` / `/login/undefined` 旧理论（路由默认就是 `/login`，
   `/login/undefined` 只是 catch-all，不是卡点本身）。
3. 把“确认按钮可点”当成“验证码已被接受”。
4. 只 patch 顶层 window 去抓 OneID 请求。
