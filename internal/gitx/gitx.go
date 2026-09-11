// Package gitx 是 git 的薄封装，只覆盖 forge 需要的四件事：
// 浅克隆、暂存、提交、推送。
//
// # 凭据怎么进去（这是本包唯一真正需要小心的地方）
//
// 三条路都不能走：
//
//   - 把 token 拼进 remote URL（`https://x-access-token:TOKEN@github.com/…`）：
//     这条 URL 会被**写进 `.git/config`** 留在工作副本里，之后任何读到那个目录的
//     步骤（含失败时上传的 artifact）都拿到了 token。
//   - 把 token 放进命令行参数：`ps` 能看到，CI 的错误输出也可能回显整个 argv。
//   - 把 token 写进临时脚本给 GIT_ASKPASS：等于把 token 落盘。
//
// 所以走 **credential.helper**：helper 的内容经 `-c` 传入（argv 里只有**变量名**），
// token 的值从**环境变量**读（env 不进 argv、不进 .git/config、不进 shell history）。
// 这是本包里唯一出现 token 的地方，且它以 `GH_TOKEN` 这个名字出现。
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// credentialHelper 让 git 从环境变量里取凭据。
//
// `x-access-token` 是 GitHub 对"用 token 当密码"约定的用户名；PAT 不校验用户名，
// 但用这个约定值能让日志里一眼看出这是 token 认证而不是某人的账号密码。
//
// 注意 `$GH_TOKEN` 是被 git 调起的 sh 展开的，不是被 Go 展开的 —— Go 侧完全不碰
// token 的值，也就无从把它写进 argv。
const credentialHelper = `!f() { echo "username=x-access-token"; echo "password=$GH_TOKEN"; }; f`

// credentialArgs 返回配置凭据助手的 `-c` 参数。
//
// **第一项 `credential.helper=` 是空串，不是笔误** —— 它把**继承来的助手列表清空**。
// 少了它，`-c credential.helper=<我们的>` 只是往列表里**追加**，于是机器上原有的助手
// （开发者机器上的 Git Credential Manager、`actions/checkout` 在 runner 上留下的
// store 助手）会**先**跑一遍：去问系统钥匙串、超时、拿到一份错的或空的凭据，
// 然后才轮到我们。实测在 Windows 上这一下就是十几秒；在 CI 上更糟——它可能
// 转身去问终端，而 runner 上无终端可问，于是 job 挂到超时，报出来的是
// "超时"而不是"凭据没送到"。
func credentialArgs() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + credentialHelper,
	}
}

// Client 是一个已经克隆好的工作副本。
type Client struct {
	// Dir 是工作副本目录。
	Dir string
	// Token 是推送用的 PAT。为空表示匿名（只读操作可用）。
	Token string
	// Log 用于记一行命令（**不含** token）。
	Log func(format string, a ...any)
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		c.Log(format, a...)
	}
}

// env 返回子进程的环境：继承当前环境，并在有 token 时补上 GH_TOKEN。
//
// 同时把 `GIT_TERMINAL_PROMPT` 关掉。这一条不是洁癖：credential helper 配错了
// （或 runner 上没有 sh）时的默认行为是 **git 转身去问终端**，而在 CI 里没有终端
// 可问，于是这个 job 会一直挂到超时为止 —— 报出来的是"超时"，与真正的原因
// （凭据没送到）毫无关系。关掉之后它立刻以 `could not read Username` 失败。
func (c *Client) env() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		// 先剔掉可能已存在的同名变量，避免"两个同名变量"这种未定义行为。
		if strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0")
	if c.Token != "" {
		out = append(out, "GH_TOKEN="+c.Token)
	}
	return out
}

// run 跑一条 git 命令。
//
// **不回显 argv，也不回显 env** —— 这是 §4.5 规则 8 在 Go 这一侧的落点：
// 出错时只报最近一次 git 的 stderr（git 自己不会打印凭据），
// 而不是把整个命令行拼进错误信息。
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	// credentialArgs 总在最前面（git 的顶层选项要在子命令之前）。
	// 即使没有 token 也要清空助手列表：不清的话 git 会去问系统钥匙串，
	// 而这在无人值守环境里就是"挂起"。
	full := append(credentialArgs(), append([]string{"-C", c.Dir}, args...)...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = c.env()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	c.logf("git %s", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		// 只带 stderr：git 的 stderr 里不会有 token（它从不打印密码），
		// 而 stdout 可能很大（比如 diff）。
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s：%s", args[0], msg)
	}
	return stdout.String(), nil
}

// Clone 浅克隆一个仓库到 dir。
//
// 只要 `--depth=1`：forge 从不看历史（对账靠的是现状而不是"上次跑到哪"，§4.4）。
// 浅克隆把一次对账的传输量从"整个仓库历史"降到"当前树"，对一个会被每天触发的
// 流程来说是数量级的差别。
//
// 不指定分支：走远端 HEAD。写死 `main` 或 `master` 会在仓库改默认分支时静默失败，
// 而这里没有任何理由关心分支叫什么。
func Clone(ctx context.Context, remote, dir, token string, log func(string, ...any)) (*Client, error) {
	c := &Client{Dir: dir, Token: token, Log: log}

	// 克隆是唯一不走 run() 的命令：它必须换一个工作目录（-C 指向的目录还不存在），
	// 所以这里单独拼一次，但凭据的传法完全一致。
	args := append(credentialArgs(), "clone", "--depth=1", "--quiet", remote, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = c.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if log != nil {
		log("git clone --depth=1 %s %s", remote, dir)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("克隆 %s：%s", remote, msg)
	}
	return c, nil
}

// Identity 是提交时用的身份。
//
// 显式给而不是靠 runner 的全局配置：Ubuntu runner 上 `user.email` 常常是空的，
// 缺了它 git 会直接拒绝提交（且错误信息是 `Please tell me who you are`，
// 与真正的问题八竿子打不着）。
type Identity struct {
	Name  string
	Email string
}

// DefaultIdentity 是 forge 回写时用的身份。
var DefaultIdentity = Identity{
	Name:  "forge",
	Email: "forge@users.noreply.github.com",
}

// HasChanges 报告工作区（含未跟踪文件）有没有改动。
func (c *Client) HasChanges(ctx context.Context, paths ...string) (bool, error) {
	args := []string{"status", "--porcelain"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Commit 暂存指定路径并提交。没有改动时返回 committed=false 且不报错。
//
// **没有改动就什么都不做**，而不是提交一个空 commit：空 commit 会让每一次
// 无事发生的对账都在 store 的历史里留下一行，把真正有意义的改动淹掉。
func (c *Client) Commit(ctx context.Context, id Identity, message string, paths ...string) (bool, error) {
	if len(paths) == 0 {
		return false, fmt.Errorf("Commit 至少要给一个路径 —— 否则会暂存整个工作区，把无关改动一起提交")
	}

	changed, err := c.HasChanges(ctx, paths...)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}

	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := c.run(ctx, addArgs...); err != nil {
		return false, err
	}

	if _, err := c.run(ctx,
		"-c", "user.name="+id.Name,
		"-c", "user.email="+id.Email,
		"commit", "--quiet", "-m", message,
	); err != nil {
		return false, err
	}
	return true, nil
}

// Push 把当前分支推到 origin。
//
// 用 `HEAD` 而不是写死的分支名：浅克隆下来时当前分支就是远端的默认分支，
// 而写死 `main`/`master` 会在仓库改名时把改动推到一个谁都不看的分支上。
func (c *Client) Push(ctx context.Context) error {
	if c.Token == "" {
		return fmt.Errorf("没有 token，不能推送")
	}
	_, err := c.run(ctx, "push", "--quiet", "origin", "HEAD")
	return err
}

// HeadSHA 返回当前 HEAD 的短 sha，用于日志里"这一轮基于哪个提交算的"。
func (c *Client) HeadSHA(ctx context.Context) string {
	out, err := c.run(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(out)
}
