# forge-core —— 私密市场的执行体

本仓库是设计文档 `03-maintenance-action.md` 里那套维护动作的**实现**（Go）与**构建**。
下文凡写 `02 §x` / `03 §x` 都是指 `02-manifest-schema.md` 与它。

**它是私有的，而且这是刻意的。** 触发面、加固清单、凭据规格都在公有的
**`market-of-labs/forge`** 那边 —— 那个仓库只有 workflow，从这个仓库的 Release 里
拉最新的二进制来执行。所以：

- **改了代码要生效，得打 tag**（见下面「发布」）。只推 commit 不影响线上。
- **打 tag = 上生产**：那个二进制会被公有的 workflow 拉下来、带着能写 `store` 的
  PAT 执行。

程序本身**不存任何状态**：每次跑都现拉 `store` 的当前状态，算完推回去。

---

## 它在做什么

事件由公有的 workflow 交给 `handle-dispatch`，它按 `$EVENT` 分派：

```
issues            → 解析 issue 正文 → 校验 → 写 sources/{appId}.json → 回评并关闭
release           → 过 §4.6 闸门 → 搬 _incoming 的 APK 到正式 Release → 清场
push              → 只对该 appId 收敛一次（解析上游 → 镜像 → 重建清单）
workflow_dispatch → 全量对账（同下）
schedule（每日）   → 全量对账：解析上游 → 补齐缺失版本 → 重建 index 与清单 → 自检 → 回写
```

**幂等 = 漏跑自愈。** 对账不依赖"上一次跑到哪"，每次都重新比对全量状态。所以某天
runner 挂了、cron 被跳过、dispatch 丢了，第二天自然补齐，不需要任何补偿逻辑。
唯一的例外是**历史不回填**（D33）：收录时只镜像*当刻的最新版本*，更老的版本不追溯。

---

## 怎么跑

需要 **Go 1.26+**。没有别的依赖 —— APK 的 `package / versionName / versionCode / ABI`
是**纯 Go 解析**的（`internal/apkmeta`），不需要 `aapt`、不需要 Android SDK。
workflow 里因此没有 "Install deps" 这一步，也就少了一个"runner 镜像换了、
aapt 装不上了"的故障面。

```bash
go build -o forge ./cmd/forge
./forge help
./forge verbs        # 一行一个子命令，给脚本消费
go test ./...
```

### 环境变量

| 变量 | 用途 |
|---|---|
| `STORE_TOKEN` | fine-grained PAT（见公有仓库的 README）。只读动词可以不给 |
| `STORE_REPO` | store 仓库 `owner/name`，默认 `market-of-labs/store` |
| `FORGE_REPO` | 公有的**运行器**仓库 `owner/name`，默认 `market-of-labs/forge` |
| `STORE_DIR` | 一个**已经 checkout 好**的 store 工作副本。留空则自己浅克隆到临时目录（用完删掉） |
| `FORGE_API_BASE` | API 根地址。Actions 里自动用 `$GITHUB_API_URL` |
| `EVENT` `ISSUE` `RELEASE_TAG` `PRERELEASE` `SHA` `REF` | `client_payload` 带过来的事件字段 |

`STORE_DIR` 那一条是给本地调试用的：指一个你的真实工作副本，动词就会直接改它，
不克隆也不删（`Close` 对"调用方给的副本"什么都不做 —— 那可能是你的工作区）。

### 子命令

| 子命令 | 干什么 |
|---|---|
| `handle-dispatch` | 按 `$EVENT` 分派（CI 的入口） |
| `intake-issue` | `-issue N` 处理一张申请单 |
| `intake-incoming` | 搬 `_incoming` 并清场（`-force` 跳过闸门，只给本地调试） |
| `resolve-upstream` | `-only ID` 只算出该镜像哪些版本并打印计划，**不下载不上传** |
| `mirror-upstream` | `-only ID` `-dry-run` 下载 → 按内容判 ABI → 改名 → 幂等上传 |
| `build-index` | `-fetch-missing` 从 Release 现状重建 `store/index.json` |
| `build-manifest` | 由 sources + index + endpoints 合成 `apps.json` |
| `check-manifest` | 跑 02 §2.8 自检 + 阈值告警 |
| `reconcile` | `-only ID` `-dry-run` 幂等全量对账（§4.4） |
| `commit-back` | `-m MSG` 按固定路径提交并推送（自动加 `[skip-dispatch]`） |

退出码：`0` 成功 · `1` 执行失败 · `2` 用法或配置错误 · `3` 自检发现硬错误。

**上线前先看一眼会被命名成什么**，这条不需要 token、不会改任何东西：

```bash
STORE_DIR=/path/to/store ./forge resolve-upstream -only com.example.app
STORE_DIR=/path/to/store ./forge mirror-upstream -only com.example.app -dry-run
```

---

## 发布

`.github/workflows/build.yml`，**只有打 tag 才构建发布**（`v*`）：

```bash
git tag v0.1.0 && git push origin v0.1.0
```

它会 `go test ./...` → `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath`
→ 把 `forge-linux-amd64` 传成该 tag 的 Release asset。**测试放在发布之前**，
不过就中止 —— 这是本仓库唯一一道闸门。平时的提交不走 CI（私有仓库的分钟数要省着），
所以跑测试是你本地的事。

两条规矩：

- **一个 tag 只对应一个二进制。** 覆写 asset 会让线上跑的代码变了而 tag 没变，事后
  无迹可查 —— 所以发布那一步写的是 `overwrite_files: false`，**存量 asset 不可能被
  替换**。改代码请打**新** tag；只有"Release 已建但 asset 没传上去"这种情况，重跑会
  把 asset 补上。**注意与老写法的差别**：现在是"已存在就跳过并打一行日志"，不是
  "报错退出" —— 不变量仍然成立（由"不可能覆盖"保证，而不是由"敢覆盖就报错"保证），
  但重跑一个已发过的 tag 会**绿着**过去，不再有那条红色提示。
- **不推分支不影响线上。** 公有的运行器浮动取 latest，所以"哪次发布生效"完全由
  tag 决定，而不是由 push 决定。

`build.yml` 里三个 action 用的都是**主版本标签**（`@v7` / `@v3`），不钉 commit SHA ——
所以 `.github/dependabot.yml` 每周开一个 PR 帮你跟新主版本，那也是这个取舍的配套：
钉 SHA 更抗供应链，但每次升级都要人工去查 SHA，而 SHA 写错是**静默的**（没有任何
东西会告诉你它指到了别处），少一道人工就多一道自动。Dependabot 只开 PR，
**不合并、不打 tag**，所以线上仍然只由你的 tag 改变；本仓库的 workflow 又只在打 tag
时触发，它开的 PR 跑不起来任何东西，**不花分钟数**。

> 发布那一步用的是第三方 action（`softprops/action-gh-release`）。这里可以接受，是因为
> **这个 job 不握 PAT** —— 它只有本仓库的 `GITHUB_TOKEN`（`contents: write`），风险被
> 限制在 `forge-core` 自己身上。反过来说：**公有的 `forge` 那三个 workflow 一个第三方
> action 都没有**，因为它们的 job 里有能写三个仓库的 PAT（§6.2）。

---

## 代码结构

```
cmd/forge/            子命令分发、环境读取、退出码
internal/
  naming/             文件名的契约：{appId}-{version}-{abi}.apk
  apkmeta/            纯 Go 读 APK：package / versionName / versionCode / ABI
  model/              清单、索引、来源、地址模板的类型与校验
  issue/              issue 正文的表单解析（纯数据，绝不 eval）
  upstream/           从上游 Release 里挑该镜像哪些文件
  manifest/           sources + index + endpoints → apps.json
  gh/                 GitHub API 封装
  gitx/               git 封装（凭据经 -c credential.helper 一次性传入）
  store/              store 工作副本的读写
  job/                把上面这些拼成"一次维护动作"（十个动词）
```

分层的规矩很简单：下面一层不知道上面一层的存在。`job` 不碰 `os.Args`、不决定退出码、
不打印 `::add-mask::` —— 那些是 `cmd` 的事，于是每个动词都能在测试里被直接调用。

程序**不依赖工作目录**：`store` 是它自己 `os.MkdirTemp` 克隆的，所以它可以在任何 cwd
下被调用（公有的 workflow 里因此没有 `working-directory`）。

### 两个关键决定

**ABI 的唯一权威是 APK 内容。** 文件名里的 ABI 只是**线索**，用来产生一条 mismatch
告警，绝不参与改名决策。所以"上游把 arm64 的包命名成 x86"这种打包错误，结果是
*我们按内容安放、同时喊一声*，而不是把一个 x86 的名字安到一个 arm64 的包上。

**文件名是硬依赖。** tag = `{appId}` 不带版本，所以 `{appId}-{version}-{abi}.apk`
是版本与 ABI 的**唯一**载体。改坏它的后果不是报错，而是清单里多一条客户端静默
解析不了的条目（02 §2.4）—— 所以 `naming.Split` 的返回值必须被检查，
不能"猜一个"。

---

## 测试

```bash
go test ./...
```

纯逻辑那几个包（`naming` / `apkmeta` / `model` / `issue` / `upstream` / `manifest`）
都是无 IO 的，测试直接调用。`job` 里被重点覆盖的是**判断**那部分
（`DecideIntake` / `pickTargets` / `CheckIncomingGate`）—— 它们同样是纯函数，
把最难的那部分（该不该做）从网络那部分（怎么做）里切了出来。

---

## 加固

触发面白名单、加固清单、凭据规格、部署前置检查 —— 都在公有仓库
`market-of-labs/forge` 的 README 里。本仓库额外两条：

- **tag 与二进制一一对应**（上面「发布」）—— 挡住"生产代码变了而 tag 没变"。
- **build 用的 action 不带 `# vX.Y.Z` 之类的 SHA 注释**，一律主版本标签 —— 升级靠
  Dependabot 开 PR，人只做 review（上面「发布」）。
