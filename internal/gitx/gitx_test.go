package gitx_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gitx"
)

const testToken = "github_pat_11ABCDEFG_git_push_secret"

// newRepo 造一个本地仓库当 fixture，并给它配一个 origin。
//
// 用本地仓库而不是远程：整组测试要能离线跑。真正需要"网络 + 认证"的那条
// （TestPushSendsTokenViaCredentialHelper）用一个本地 HTTP 服务器假扮远端，
// 于是既在离线可跑，又真的走了 git 的 HTTP 认证路径。
func newRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s：%v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "seed.txt")
	run("commit", "--quiet", "-m", "seed")
	return dir
}

// TestCommitSkipsWhenNothingChanged 钉住"无事发生就不提交"。
//
// 一次什么都没改的对账如果也留一个空 commit，store 的历史会被每天一行
// "no changes"淹掉，真正有意义的改动再也 diff 不出来。
func TestCommitSkipsWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	c := &gitx.Client{Dir: dir}

	committed, err := c.Commit(ctx, gitx.DefaultIdentity, "chore: 无事发生", ".")
	if err != nil {
		t.Fatalf("Commit：%v", err)
	}
	if committed {
		t.Error("没有改动时不该产生提交")
	}
}

func TestCommitCreatesCommitWithMessage(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &gitx.Client{Dir: dir}
	committed, err := c.Commit(ctx, gitx.DefaultIdentity, "chore: 重建清单 [skip-dispatch]", "apps.json")
	if err != nil {
		t.Fatalf("Commit：%v", err)
	}
	if !committed {
		t.Fatal("有改动却没提交")
	}

	out, err := exec.Command("git", "-C", dir, "log", "-1", "--pretty=%B").Output()
	if err != nil {
		t.Fatal(err)
	}
	msg := string(out)
	if !strings.Contains(msg, "重建清单") {
		t.Errorf("提交信息里没有我们的内容：%q", msg)
	}
	// 规则 7 靠这个标记抑制自激，而判定它是**子串匹配**（`contains(...)`），
	// 所以只要在信息里出现即可 —— 但必须在，否则一次回写会触发一次 dispatch，
	// 无限循环。
	if !strings.Contains(msg, "[skip-dispatch]") {
		t.Errorf("提交信息里没有 [skip-dispatch]：%q", msg)
	}
}

// TestCommitRejectsEmptyPaths 钉住"必须显式给路径"。
//
// 不给路径就 `git add .` 等于把工作区里的一切（包括不该提交的临时文件）
// 一起带上；而 commit-back 是**唯一**的回写出口，它出错的代价是整个 store 仓库。
func TestCommitRejectsEmptyPaths(t *testing.T) {
	c := &gitx.Client{Dir: newRepo(t, "")}
	if _, err := c.Commit(context.Background(), gitx.DefaultIdentity, "x"); err == nil {
		t.Error("不给路径应当报错，而不是暂存整个工作区")
	}
}

func TestCommitSetsIdentity(t *testing.T) {
	// runner 上 user.email 常是空的，缺了它 git 直接拒绝提交。
	// 这里用一个"当前仓库没有的"身份，验证它确实是被 -c 带进去的。
	ctx := context.Background()
	dir := newRepo(t, "")
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &gitx.Client{Dir: dir}
	id := gitx.Identity{Name: "forge-bot", Email: "bot@example.invalid"}
	committed, err := c.Commit(ctx, id, "chore: x", "x.json")
	if err != nil {
		t.Fatalf("Commit：%v", err)
	}
	if !committed {
		t.Fatal("有改动却没提交")
	}
	out, _ := exec.Command("git", "-C", dir, "log", "-1", "--pretty=%an <%ae>").Output()
	if got := strings.TrimSpace(string(out)); got != "forge-bot <bot@example.invalid>" {
		t.Errorf("提交者 = %q", got)
	}
}

// TestPushSendsTokenViaCredentialHelper 是本包最重要的一条测试。
//
// 它拿一个**要求 Basic 认证**的本地服务器假扮 GitHub，然后真的调 Push，验证：
//   - git 确实向 credential helper 要了凭据（而不是转身去问终端）；
//   - 送到服务器上的用户名/密码就是我们期望的那对；
//   - 于是"token 走 env、不经 argv、不落 .git/config"这条设计是真的能跑通的。
//
// 为什么必须有这条：credential helper 配错时的表现不是报错，而是 **git 挂起等输入**。
// 在 CI 里那就是一个跑到超时、且错误信息与真实原因毫无关系的 job。
func TestPushSendsTokenViaCredentialHelper(t *testing.T) {
	var mu sync.Mutex
	var sawAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		sawAuth = append(sawAuth, auth)
		mu.Unlock()

		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))
		if auth != want {
			// 第一次没带凭据是正常的：git 要先被 401 撞一下才知道要认证。
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 认证过了。这里不实现 git-upload-pack 协议 —— 我们只关心凭据有没有送到，
		// 所以直接用一个服务端错误结束，push 会失败，那是预期的。
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := newRepo(t, srv.URL+"/repo.git")
	c := &gitx.Client{Dir: dir, Token: testToken}

	// push 会失败（服务器没实现 receive-pack），这不重要 —— 重要的是它失败在
	// **认证之后**。若 helper 没生效，失败会发生在认证阶段，且服务器看不到正确凭据。
	_ = c.Push(context.Background())

	mu.Lock()
	defer mu.Unlock()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))

	// 只允许两次请求：一次不带凭据（被 401 撞一下，git 才知道要认证），
	// 一次带着我们的凭据。
	//
	// 上限卡死在这里是**故意的**：机器上（和 runner 上）常常已经配了别的凭据助手，
	// 而 `-c credential.helper=X` 只是**追加**而不是替换 —— 不清空继承列表的话，
	// 那些助手会先跑一遍（去问系统钥匙串、超时、给出错的凭据），表现是多出好几个
	// 空 Authorization 请求，以及十几秒的额外耗时；最坏情况是它转身去问终端，
	// 在 CI 上挂到超时。这条断言就是钉住 credentialArgs() 里那个清空用的空串。
	if len(sawAuth) > 2 {
		t.Errorf("服务器收到 %d 次请求 %q —— 只该有「一次 401 探测 + 一次带凭据」，"+
			"多出来的多半是继承来的凭据助手在跑（credential.helper= 的空串清空没了？）",
			len(sawAuth), sawAuth)
	}
	if len(sawAuth) == 0 {
		t.Fatal("服务器一个请求都没收到 —— push 根本没走到 HTTP 层")
	}
	if got := sawAuth[len(sawAuth)-1]; got != want {
		t.Errorf("最后一次请求的 Authorization = %q，期望 %q。收到的依次是 %q",
			got, want, sawAuth)
	}
}

// TestTokenNeverLandsInGitConfig 钉住"不改 remote URL"。
//
// 把 token 拼进 remote URL 是最省事的写法，也是把长期凭据**留在工作副本里**的写法：
// 之后任何读到那个目录的步骤（失败时上传的 artifact、后续的 build 步骤）都拿到了它。
func TestTokenNeverLandsInGitConfig(t *testing.T) {
	dir := newRepo(t, "https://example.invalid/repo.git")
	c := &gitx.Client{Dir: dir, Token: testToken}

	// 触发一次会用凭据的操作（会失败，无所谓）。
	_ = c.Push(context.Background())

	cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), testToken) {
		t.Errorf(".git/config 里出现了 token：\n%s", cfg)
	}
	// helper 是经 -c 一次性传入的，不该被持久化到配置里。
	if strings.Contains(string(cfg), "credential.helper") {
		t.Errorf(".git/config 里被写进了 credential.helper：\n%s", cfg)
	}
}

// TestCommitToleratesMissingPath 钉住空仓库里的第一次回写。
//
// `apps.json` 在**第一条来源落地之前**是不存在的，而回写的路径列表照旧带着它。
// `git add` 是全有或全无的：那个不存在的 pathspec 会让整条命令 fatal，于是
// "第一个被收录的应用"永远提交不了 —— 报出来的还是一句看不出所以然的
// `pathspec 'apps.json' did not match any files`（2026-09-12 线上就是这条）。
func TestCommitToleratesMissingPath(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sources", "a.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &gitx.Client{Dir: dir}
	committed, err := c.Commit(ctx, gitx.DefaultIdentity, "收录 a（#1）", "sources", "apps.json")
	if err != nil {
		t.Fatalf("Commit：%v", err)
	}
	if !committed {
		t.Fatal("有改动却没提交")
	}
	out, _ := exec.Command("git", "-C", dir, "show", "--name-only", "--pretty=", "HEAD").Output()
	if !strings.Contains(string(out), "sources/a.json") {
		t.Errorf("提交里没有新来源：%s", out)
	}
}

// TestCommitStillStagesDeletion 是上一条的另一半：摘掉路径的判据不能把**删除**也摘掉。
//
// 已跟踪但工作区里没有的路径是一次删除，`git add` 会正常把它暂存；若因为
// "文件不存在"就一并跳过，那次删除会永远留在工作区、永远提交不上去。
func TestCommitStillStagesDeletion(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	if err := os.Remove(filepath.Join(dir, "seed.txt")); err != nil {
		t.Fatal(err)
	}

	c := &gitx.Client{Dir: dir}
	committed, err := c.Commit(ctx, gitx.DefaultIdentity, "移除 seed", "seed.txt")
	if err != nil {
		t.Fatalf("Commit：%v", err)
	}
	if !committed {
		t.Fatal("删除已跟踪文件却没提交")
	}
	out, _ := exec.Command("git", "-C", dir, "show", "--name-status", "--pretty=", "HEAD").Output()
	if !strings.Contains(string(out), "D\tseed.txt") {
		t.Errorf("提交里没有那次删除：%s", out)
	}
}

func TestHasChanges(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	c := &gitx.Client{Dir: dir}

	changed, err := c.HasChanges(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("干净的工作区不该报有改动")
	}

	if err := os.WriteFile(filepath.Join(dir, "new.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = c.HasChanges(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("新增了未跟踪文件，应当报有改动")
	}
}

// TestHasChangesScopedToPaths 钉住"按路径判断"。
//
// 若不看路径，某个步骤在仓库里留下的无关临时文件会让 commit-back 认为"有改动"，
// 于是空跑一次提交流程 —— 更糟的是接着 `git add` 那些路径之外的东西。
func TestHasChangesScopedToPaths(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t, "")
	c := &gitx.Client{Dir: dir}

	if err := os.WriteFile(filepath.Join(dir, "unrelated.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := c.HasChanges(ctx, "apps.json")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("apps.json 没变，不该因为存在无关文件就报有改动")
	}
}

func TestPushWithoutTokenFails(t *testing.T) {
	// 没 token 就别假装能推 —— 报一条明确的错，而不是让 git 去问终端。
	c := &gitx.Client{Dir: newRepo(t, "https://example.invalid/x.git")}
	if err := c.Push(context.Background()); err == nil {
		t.Error("没有 token 时推送应当直接失败")
	}
}

func TestCloneRejectsMissingRemote(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := gitx.Clone(ctx, filepath.Join(t.TempDir(), "does-not-exist"), dir, "", nil)
	if err == nil {
		t.Fatal("克隆一个不存在的仓库应当报错")
	}
	// 错误里要带上远端地址，否则多仓库流程里看不出是哪一步炸的。
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("错误信息里没有远端地址：%v", err)
	}
}
