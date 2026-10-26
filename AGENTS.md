# client2api 项目约定

## 位置

- 源码：`D:\client2api-src`（只放代码）
- 正式部署：`D:\client2api`（安装版，服务跑在这里，账号数据在 `D:\client2api\data`）
- 构建工具链：`D:\client2api-lab\_tools`（Go、w64devkit、NSIS）

## 部署约定

- 网关从 `D:\client2api` 跑，端口看 `D:\client2api\configs\client2api.json`
  里的 `listen`（2026-10-03 迁移后是 `127.0.0.1:8790`）；它在托盘菜单的设置里改。
- 不要在源码目录 `D:\client2api-src` 直接跑 `client2api.exe`：那里的 `data\` 是
  迁移前的旧快照，跑起来会和正式部署分叉，两个实例还会互相抢端口。
- 升级是**用户手动**去装新的 `dist\client2api-setup-<version>.exe`：程序文件替换，
  `configs\client2api.json` 和 `data\` 原地保留。
- 安装器会 `taskkill /IM client2api.exe`（按进程名），所以用户升级前不用手动停服务，
  但源码目录那个开发实例也会一起被杀。
- **不要碰正在跑的服务**（2026-10-03 起）：不要 stop / restart / 杀掉 `D:\client2api`
  里的实例，也不要跑安装包去覆盖它，更不要为了验证去重启它。那个服务用户正在用，
  更新时机由用户自己决定；我们只负责产出安装包。
- 需要在运行中的实例上验证或改数据时，走面板 API（`http://127.0.0.1:8790/panel/api/...`），
  它们热生效：例如导账号池用
  `POST /panel/api/clients/<id>/import/bundle`（body 是 cockpit 数组，`expires_at` 单位
  毫秒），导入后模块自己重建账号池，不需要重启进程。

## 构建环境

每个新 shell 先设置：

```powershell
$tools='D:\client2api-lab\_tools'
$env:PATH="$tools\go\bin;$tools\w64devkit\bin;$env:PATH"
$env:GOCACHE="$tools\gocache"; $env:GOTMPDIR="$tools\gotmp"; $env:GOMODCACHE="$tools\gomodcache"
$env:GOFLAGS='-mod=mod'; $env:GOPROXY='off'; $env:CGO_ENABLED='0'
Set-Location 'D:\client2api-src'
```

## 工作流

1. 用户说改功能：只改 `D:\client2api-src` 的源码，用 `go build ./...` 和 `go test ./...`
   验证后再回报。

2. 用户说打包／要给新的 exe：先提升版本号，再构建安装包。
   - 版本号只有一处来源：`cmd/client2api/main.go` 里的 `var version = "..."`。
     `installer/setup/main.go` 的同名字面量必须跟着改，`go test ./installer/...`
     会断言两者一致（`installer/version_test.go`）。
   - 打包命令：`pwsh -File installer\build.ps1`，产物是
     `dist\client2api-setup-<version>.exe`。
   - 只产出安装包，不要执行安装包；用户会自己选时间升级。

3. 用户安装新 exe 属于升级：程序文件被替换，`configs\client2api.json` 和 `data\`
   原样保留（配置、账号池、用量历史都不丢）。这条由
   `installer/setup/install.go` 的 `writePayload` 保证，
   `installer/setup/install_test.go` 覆盖，不要改回覆盖式写入。

## 打包给别人的包

- 要分发的包用空包：`pwsh -File installer\build.ps1 -NoData`
  （示例配置、不含账号数据），对方用面板的导入功能恢复自己的数据。
- 不带 `-NoData` 打出来的包会把当前账号数据一起打进去，不要外发。


## 发版（Release）

- 发布流程是**全自动**的：`git tag vX.Y.Z` + `git push origin vX.Y.Z`，
  `.github/workflows/go-binaries.yml` 会跑门禁（gofmt / panel smoke / vet /
  test / -race）并自动创建 GitHub Release——标题 `client2api vX.Y.Z`，
  正文从 `CHANGELOG.md` 对应小节生成（`### Added/Fixed/…` 自动翻成
  `## 新增/修复/…`），并附五平台便携版 + `checksums.txt`。
  **绝不手动 `gh release create`**：手动建会把标题/正文格式顶掉，还会和
  流水线抢时序。
- tag 前先本地过一遍和 CI 相同的门禁，尤其是 `gofmt -l`（按仓库 blob 校验，
  本地 autocrlf 检出会误报，用 `git cat-file blob HEAD:<file> | gofmt -l`
  这类字节级校验）。**写文件不要用 PS 5.1 的 `Set-Content -Encoding UTF8`**
  （带 BOM），用 python utf-8 或 `[System.IO.File]::WriteAllText`。
- Release 上挂的 Windows 安装包必须是 **`-NoData` 空包**（仓库是 public 的，
  数据包含全部账号凭据）。空包在对应 tag 的 worktree 里单独构建后
  `gh release upload`，同时把它的 SHA256 回填进 `CHANGELOG.md` 对应小节
  （正文生成自 CHANGELOG，必须先改再打 tag）。
- 版本号只有 `cmd/client2api/main.go` 一处真源，installer 侧跟改；
  `tag == 源码 version` 有 CI 断言。

## 其他

- 面板 UI 在 `internal/panel/index.html`。
- 网关配置格式和每个模块的行为写在 `README.md`，改行为时同步更新它。
