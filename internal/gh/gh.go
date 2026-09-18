// Package gh 是 forge 需要的那个最小 GitHub API 子集。
//
// 为什么自己写而不是调 `gh` CLI 或引一个 SDK：
//
//   - **不经过 shell**。03 §4.5 规则 8 要求"禁 set -x、禁 curl -v、token 绝不拼进 URL"——
//     那些都是 shell 拼接特有的泄密面。token 放进 http.Header 之后，这个面直接不存在
//     （Go 的错误里只有 URL，没有 header）。
//   - **可单测**。BaseURL 可指向 httptest，所以"幂等闸门""分页""404 语义"这些
//     真正容易写错的地方都能被测试钉住，而不是等 CI 上跑一次才知道。
//   - **依赖为零**。这个仓库要在 runner 上每次跑，多一个依赖就多一份供应链面。
//
// 本包只做 HTTP：不认识 apps.json，也不认识 sources/ 里那些 JSON。
package gh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 默认端点。
const (
	DefaultBaseURL       = "https://api.github.com"
	DefaultUploadBaseURL = "https://uploads.github.com"
	// APIVersion 是 X-GitHub-Api-Version。写死而不是用 latest：规范承诺这个值下的
	// 响应形状稳定，而 `latest` 会在 GitHub 改版时悄悄换掉我们依赖的字段。
	APIVersion = "2022-11-28"
)

// ErrNotFound 是 404 的哨兵。调用方要区分"这个 Release 还不存在（该建）"与
// "网络/权限出问题了（该失败）"—— 两者混在一起会让幂等逻辑变成"每次重建 Release"。
var ErrNotFound = errors.New("资源不存在（404）")

// Config 是客户端配置。
type Config struct {
	// Token 是 fine-grained PAT（03 §6.1）。留空则匿名请求 —— 解析公开上游时
	// 匿名也能用，但限额是 60 次/小时；带上 token 是 5000。
	Token string
	// BaseURL / UploadBaseURL 留空则用默认值。测试指向 httptest。
	BaseURL       string
	UploadBaseURL string
	// HTTPClient 留空则用一份带合理超时的默认客户端。
	HTTPClient *http.Client
	// UserAgent 会写进请求头。GitHub 对无 UA 的请求会拒。
	UserAgent string
}

// Client 是 API 客户端。
type Client struct {
	token         string
	baseURL       *url.URL
	uploadBaseURL *url.URL
	http          *http.Client
	userAgent     string
}

// New 造一个客户端。
func New(cfg Config) (*Client, error) {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	bu, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("BaseURL %q 解析失败：%w", base, err)
	}
	up := cfg.UploadBaseURL
	if up == "" {
		up = DefaultUploadBaseURL
	}
	uu, err := url.Parse(up)
	if err != nil {
		return nil, fmt.Errorf("UploadBaseURL %q 解析失败：%w", up, err)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		// 不设 Client.Timeout：下载 asset 可能是几十 MB，一个总超时会误杀。
		// 用传输层的分段超时，这样"连不上"与"传得慢"被区分开。
		hc = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				// 连不上的超时压到 15s：被墙住的 runner 不该让整个对账挂在那里。
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		}
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "market-of-labs-forge"
	}
	return &Client{token: cfg.Token, baseURL: bu, uploadBaseURL: uu, http: hc, userAgent: ua}, nil
}

// ---- 领域类型 ---------------------------------------------------------------

// Release 是 Release 里我们关心的字段（03 §3.1）。
type Release struct {
	ID          int64   `json:"id"`
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	PublishedAt string  `json:"published_at"`
	CreatedAt   string  `json:"created_at"`
	Body        string  `json:"body"`
	Assets      []Asset `json:"assets"`
}

// Asset 是一个 Release asset。
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Issue 是 issue 里我们关心的字段（03 §2.5）。
type Issue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	HTMLURL     string `json:"html_url"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

// ReleasePatch 是 PATCH release 的可选字段（用指针区分"不改"与"改成零值"）。
type ReleasePatch struct {
	Draft      *bool   `json:"draft,omitempty"`
	Prerelease *bool   `json:"prerelease,omitempty"`
	Name       *string `json:"name,omitempty"`
	// TagName 改 Release 的 tag 名。**发 draft 的时候必须一并带上它**，否则 GitHub 会把
	// tag 名换成一个 `untagged-<sha>` 占位名 —— 见 Unpublish 的说明。
	//
	// 对 draft 改 tag 名不会 422，即使同名的 ref 已经存在（2026-09-16 在 store 的队列上
	// 实测：`refs/tags/_incoming` 在改之前就在）。ref 与 Release 是两回事，各自独立。
	TagName *string `json:"tag_name,omitempty"`
	// Body 是 Release 正文（description）。用来把上游 README 同步过去（D51）——
	// 改正文不产生状态跃迁，所以不会触发 `release` 事件（03 §3.2）。
	Body *string `json:"body,omitempty"`
	// MakeLatest 对应 API 的 make_latest（"true"/"false"/"legacy"）。
	// 规则 4：内部 Release 操作一律 "false" —— 每次 draft→published 都会重打
	// published_at，用 false 才不会让 Release 跳到列表顶部、让 /releases/latest 抖动。
	MakeLatest *string `json:"make_latest,omitempty"`
}

// ---- 请求骨架 ---------------------------------------------------------------

// do 发一个请求并解 JSON 到 out（out 为 nil 则丢弃响应体）。
func (c *Client) do(ctx context.Context, method, rawURL string, body any, out any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码请求体：%w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// 刻意不把 req 整个塞进错误：错误会被日志打印，而 req 里带着 header。
		return nil, c.errf(rawURL, "%s %s：%v", method, redactURL(rawURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		// 这一条刻意保留 %w：调用方要靠 errors.Is 区分"资源不存在"与"其他失败"，
		// 而它的文本完全由我们拼成，不含任何外部内容，无需抹除。
		return resp, fmt.Errorf("%s %s：%w", method, redactURL(rawURL), ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		// 读一小段响应体当上下文就够定位问题；不设上限的话一个错误页可能几 MB。
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return resp, c.errf(rawURL, "%s %s：HTTP %d：%s",
			method, redactURL(rawURL), resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, c.errf(rawURL, "%s %s：解析响应：%v", method, redactURL(rawURL), err)
		}
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	return resp, nil
}

// errf 拼一条错误信息，并对**拼好之后的整段文本**做一次凭据抹除。
//
// 为什么要抹整段而不是只抹我们自己写进去的那几个值：要抹的东西可能来自任何一段，
// 而且有两处是我们控制不了的 ——
//
//   - 响应体。github.com 的错误页、以及链路上的中间代理，都可能把收到的
//     Authorization 回显出来。（测试里就构造了这种情况。）
//   - `http.Client.Do` 失败时返回的 `*url.Error`，它会把自己的 message 写成
//     `Get "http://…/releases?per_page=100": dial tcp …` —— **完整 URL 被拼进去了**，
//     而 redactURL 只作用于我们自己拼的字符串，管不到它内部。
//
// 所以这里做两件事：token 换成 `***`，以及把出现过的原始 URL 换成脱敏版。
//
// 刻意用 errors.New 而不套 %w：链路断在这里。唯一的可判断哨兵是 ErrNotFound，
// 它在自己的分支里单独用 %w 返回（见上），不受影响；而传输层错误对 forge 全是终止性的，
// 保留一个没人读的 Unwrap 链换不来什么好处。若将来需要重试或区分超时，
// 把这里换成自定义的 Unwrap 包装即可，调用点不用动。
func (c *Client) errf(rawURL, format string, a ...any) error {
	msg := c.scrub(fmt.Sprintf(format, a...))
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, redactURL(rawURL))
	}
	return errors.New(msg)
}

// scrub 把一段将被日志打印的文本里的 token 抹掉。
//
// 为什么不能指望 GitHub 自动打码（03 §4.5 规则 9）：它的自动打码表只认
// `ghp_ / gho_ / ghu_ / ghs_ / ghr_` 这几个前缀，**不认我们用的 `github_pat_`**。
// 而公有仓库的 Actions 日志任何登录用户都能读，所以这一步是硬要求，不是洁癖。
func (c *Client) scrub(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "***")
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		// token 只在这里出现一次。绝不拼进 URL（URL 会进日志、进 http 错误、进 Referer）。
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// redactURL 去掉 URL 里的 userinfo 与 query —— 万一有人把 token 放进 URL，
// 至少错误信息不会把它带进日志。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}

// ---- Release ----------------------------------------------------------------

// GetRelease 按 tag 取 Release。不存在时返回 ErrNotFound。
func (c *Client) GetRelease(ctx context.Context, repo, tag string) (*Release, error) {
	u := c.repoURL(repo, "releases", "tags", tag)
	var r Release
	if _, err := c.do(ctx, http.MethodGet, u, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetReleaseByID 按 id 取 Release。
func (c *Client) GetReleaseByID(ctx context.Context, repo string, id int64) (*Release, error) {
	u := c.repoURL(repo, "releases", strconv.FormatInt(id, 10))
	var r Release
	if _, err := c.do(ctx, http.MethodGet, u, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListReleases 列出全部 Release（自动翻页）。
//
// per_page 取 100（上限）：对账要遍历上游的所有 Release，用默认的 30 会让
// 一个发版频繁的上游多花三四倍请求，而匿名限额只有 60/小时。
func (c *Client) ListReleases(ctx context.Context, repo string) ([]Release, error) {
	var all []Release
	next := c.repoURL(repo, "releases") + "?per_page=100"
	for next != "" {
		var page []Release
		resp, err := c.do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextLink(resp)
	}
	return all, nil
}

// LatestRelease 取上游最新发布（03 §5.4：公开上游匿名调 /releases/latest 即可）。
//
// 注意：上游若全是 draft/pre-release，这个端点会 404 —— 那不是错误，是"上游还没发布"，
// 所以这里把 ErrNotFound 正常化。
func (c *Client) LatestRelease(ctx context.Context, repo string) (*Release, error) {
	u := c.repoURL(repo, "releases", "latest")
	var r Release
	if _, err := c.do(ctx, http.MethodGet, u, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRelease 新建 Release（03 §3.1：只在**首次收录**时跑）。
func (c *Client) CreateRelease(ctx context.Context, repo, tag, name string, draft bool) (*Release, error) {
	body := map[string]any{"tag_name": tag, "name": name, "draft": draft}
	var r Release
	if _, err := c.do(ctx, http.MethodPost, c.repoURL(repo, "releases"), body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRelease 改 Release（本项目只用于"改回 draft"这条清场路径，03 §3.2）。
func (c *Client) UpdateRelease(ctx context.Context, repo string, id int64, patch ReleasePatch) (*Release, error) {
	var r Release
	if _, err := c.do(ctx, http.MethodPatch, c.repoURL(repo, "releases", strconv.FormatInt(id, 10)), patch, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Unpublish 把 Release 改回 draft，并把它本来该有的 tag 名一并带回去。
//
// **这是清场的唯一手段，绝不用 DELETE**（03 §4.5 规则 6）：删 Release 会让 tag 消失，
// 而若该仓库曾开启 Immutable Releases，删除会**永久烧毁该 tag**
// （再建同名会 422 `tag_name was used by an immutable release`）—— 而 tag = {appId}
// 是不可重建的安装身份。
//
// 前提"published → draft 走得通"已在真环境验证过（2026-09-16），不再是纸面推断。
//
// ⚠️ **tag 必须传，而且正是这一步非传不可**：PATCH 一个 draft 时若不带 `tag_name`，
// GitHub 会把 tag 名换成一个 `untagged-<sha>` 占位名 —— 和它原来叫什么无关，**哪怕它
// 本来就已是 draft**（2026-09-16 在 store 的队列上实测，PATCH 前后各做了一次独立 GET
// 复核：`_incoming` → `untagged-eea0ca81…`）。这条占位名有两个后果：
//
//   - 队列是按 tag 名认的（job.incomingRelease），名字一掉就**再也认不出来**，人传上去
//     的 APK 停在队列里不动，而运行还是绿的 —— 一天里连撞两次的"传了却没反应"；
//   - 它**不会自愈**：下一次搬运照样认不出，于是队列成了单次使用的东西。
//
// 带上 tag 名就没有这个问题（同一次实测：三个字段一起发的 PATCH，tag 名稳住了）。
// 顺手记下另一条实测：对 draft 改 tag 名**不会** 422，即使同名 ref 已存在 —— ref 与
// Release 各自独立，别被"tag 已存在"吓住。
//
// 调用方**必须把这里的错误当成要紧事报出去**，不能只记一行日志 —— 见 job.cleanIncoming。
//
// make_latest 一律 "false"（规则 4）。
func (c *Client) Unpublish(ctx context.Context, repo string, id int64, tag string) (*Release, error) {
	draft := true
	no := "false"
	return c.UpdateRelease(ctx, repo, id, ReleasePatch{Draft: &draft, MakeLatest: &no, TagName: &tag})
}

// ---- 仓库内容 ----------------------------------------------------------------

// Readme 取仓库的 README 正文（markdown 原文）。
//
// 走默认的 JSON 媒体类型 + 自己解 base64，而不是切到 `application/vnd.github.raw`：
// 后者要绕开 do() 另写一条请求路径，而它换来的只有省下 33% 的传输量。
//
// **没有 README 时返回空串且不报错** —— 仓库可以没有 README，那是正常状态而非失败；
// 调用方拿到空串就什么都不做。
func (c *Client) Readme(ctx context.Context, repo string) (string, error) {
	var r struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.repoURL(repo, "readme"), nil, &r); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if r.Encoding != "base64" {
		return "", fmt.Errorf("README 的 encoding 是 %q（只认 base64）", r.Encoding)
	}
	if r.Content == "" {
		// 1MB 以上的文件在这个媒体类型下 content 是空的，只能走 raw。
		return "", fmt.Errorf("README 内容为空（超过 1MB 的文件只走 raw 媒体类型）")
	}
	// base64.NewDecoder 会**忽略换行**：GitHub 的 base64 每 60 个字符插一个 \n，
	// 直接用 base64.StdEncoding.DecodeString 会当场报错。
	b, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(r.Content)))
	if err != nil {
		return "", fmt.Errorf("解 README 的 base64：%w", err)
	}
	return string(b), nil
}

// ---- Asset ------------------------------------------------------------------

// ListAssets 列出某个 Release 的全部 asset（自动翻页）。
//
// 走 Release 对象自带的 assets 只给前 30 个（03 §3.3：单个 Release 可以上千个 asset），
// 所以这里单独打 assets 端点并翻页。
func (c *Client) ListAssets(ctx context.Context, repo string, releaseID int64) ([]Asset, error) {
	var all []Asset
	next := c.repoURL(repo, "releases", strconv.FormatInt(releaseID, 10), "assets") + "?per_page=100"
	for next != "" {
		var page []Asset
		resp, err := c.do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextLink(resp)
	}
	return all, nil
}

// UploadAsset 上传一个 asset。
//
// 调用方负责**幂等**：上传前先 ListAssets 看目标名在不在（03 §3.1：幂等按 asset 名判断，
// **禁止 --clobber** —— 覆盖会让正在下载的客户端拿到半个文件）。
// 本函数不替它做这件事，因为"该不该跳过"是业务判断。
//
// ⚠️ `size` 必须如实给：uploads.github.com **拒收 chunked 请求体**，回的是
// `400 {"message":"Bad Content-Length"}`。这里曾经用 io.Pipe 拼 multipart 边算边发 ——
// 长度不可知，Go 就退化成 chunked，于是**一个 asset 都没上传成功过**：Release 建出来了、
// 里面永远空着，而错误在镜像侧被 D44 容忍成一条 ERROR 而已（见 MirrorUpstream）——
// 整轮照样是绿的。
// multipart 的长度还得自己算，所以这里直接发裸 body（GitHub 文档里的 `--data-binary @file`），
// 名字仍然走 query。传错了会失败得很响：Go 在 body 长度与 Content-Length 对不上时报错，
// 不会截断成半个文件 —— 那是这条路径唯一不能出的错（03 §3.1）。
func (c *Client) UploadAsset(ctx context.Context, repo string, releaseID int64, name string, content io.Reader, size int64) (*Asset, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s",
		c.uploadBaseURL.String(), repo, releaseID, url.QueryEscape(name))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, content)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.errf(u, "上传 asset %s：%v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, c.errf(u, "上传 asset %s：HTTP %d：%s", name, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var a Asset
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, c.errf(u, "上传 asset %s：解析响应：%v", name, err)
	}
	return &a, nil
}

// DeleteAsset 删除一个 asset。
//
// 只用在 `_incoming` 的清场（03 §3.2）—— 那份 Release 是暂存队列，asset 搬走后必须删掉，
// 否则下次 Publish 会重复解析。**正式 Release 的 asset 永不删除**（D13 全保留）。
func (c *Client) DeleteAsset(ctx context.Context, repo string, assetID int64) error {
	_, err := c.do(ctx, http.MethodDelete, c.repoURL(repo, "releases", "assets", strconv.FormatInt(assetID, 10)), nil, nil)
	return err
}

// DeleteTagRef 删掉一个 tag 引用（只动引用，不碰 Release）。
//
// 只用在 `_incoming` 的清场（03 §3.2）。非得删的理由不是洁癖，是 `release` 事件解析
// workflow 的方式：**它跑的是 tag 所指提交上的那份文件，不是默认分支 HEAD 上的**。
// 而这个引用一旦建立就再也不动 —— 实测撞上过：`_incoming` 的引用建于 09-12，09-16
// 的发布还在用它（两次 run 的 head_sha 都停在四天前那个提交上）。于是默认分支上给
// `intake-incoming.yml` 改触发器，队列这条路**永远看不见**：Publish 会触发一个事件、
// 解析到一份没有该触发器的 workflow、然后什么都不发生，而 Actions 页面干干净净 —— 与"传了却没
// 反应"是同一类故障，且更难查。删掉之后，下一次 Publish 会在**当时的**默认分支 HEAD
// 上把它重建出来（`target_commitish` 是分支名 `master`，不是 sha，所以每次都取当下），
// 跑的就永远是最新那份。
//
// 没有这个引用是**常态**（队列常驻 draft 时就没有），所以 404 不算失败。
func (c *Client) DeleteTagRef(ctx context.Context, repo, tag string) error {
	// 逐段传 "git"/"refs"/"tags"/tag：repoURL 会对每一段做 PathEscape，把
	// "tags/_incoming" 整段塞进去会把那个斜杠转义成 %2F，就不是 GitHub 认的端点了。
	_, err := c.do(ctx, http.MethodDelete, c.repoURL(repo, "git", "refs", "tags", tag), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// DownloadAsset 下载 asset 内容。调用方负责 Close。
//
// 走 asset 的 API URL 而不是 browser_download_url：后者会 302 到
// objects.githubusercontent.com，虽然也能用，但走 API 时 Authorization 头由我们控制，
// 且不受"browser 下载需要登录"这类策略影响。
func (c *Client) DownloadAsset(ctx context.Context, repo string, assetID int64) (io.ReadCloser, int64, error) {
	u := c.repoURL(repo, "releases", "assets", strconv.FormatInt(assetID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	c.setHeaders(req)
	// 要二进制内容，不要 JSON 元数据。
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, c.errf(u, "下载 asset %d：%v", assetID, err)
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, 0, c.errf(u, "下载 asset %d：HTTP %d：%s", assetID, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.Body, resp.ContentLength, nil
}

// ---- Issue ------------------------------------------------------------------

// GetIssue 读一个 issue（03 §4.3：payload 只带编号，正文由 forge 自己来读）。
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	u := c.repoURL(repo, "issues", strconv.Itoa(number))
	var is Issue
	if _, err := c.do(ctx, http.MethodGet, u, nil, &is); err != nil {
		return nil, err
	}
	return &is, nil
}

// CommentIssue 在 issue 上回评（03 §2.5 规则 6：处理完回评结果并关闭，拒绝也要写清原因）。
func (c *Client) CommentIssue(ctx context.Context, repo string, number int, body string) error {
	u := c.repoURL(repo, "issues", strconv.Itoa(number), "comments")
	_, err := c.do(ctx, http.MethodPost, u, map[string]string{"body": body}, nil)
	return err
}

// CloseIssue 关闭 issue。
func (c *Client) CloseIssue(ctx context.Context, repo string, number int) error {
	u := c.repoURL(repo, "issues", strconv.Itoa(number))
	_, err := c.do(ctx, http.MethodPatch, u, map[string]string{"state": "closed"}, nil)
	return err
}

// ---- 提交 --------------------------------------------------------------------

// commitFile 是 GET /repos/{o}/{r}/commits/{sha} 里我们关心的那个字段。
type commitFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"` // added / modified / removed / renamed
}

type commitDetail struct {
	Files []commitFile `json:"files"`
}

// CommitFiles 返回一次提交里改动过的文件路径（相对仓库根）。
//
// 用途只有一个：`push` 分支要"只对该 appId 做一次收敛"（03 §4.3），
// 而 dispatch 的 payload 里带的只是 sha —— 事件里没有"改了哪些文件"这个信息，
// 只能反查。为此多打一次 API 是值得的：一个来源文件的编辑不该引发一次全量对账
// （那会去打每一个上游的 API）。
//
// **翻页**：GitHub 的 commits 端点默认只回最多 300 个文件的第一页。一次手改
// 通常就一两个文件，但"批量导入 200 个来源"的提交会超 —— 所以这里跟着 Link 头翻完。
// 翻丢了不会出错，只会退化成"没识别出该收敛哪个 appId"，而调用方对此的兜底是
// 全量对账（见 job.HandleDispatch 的 push 分支），所以这里失败不致命。
func (c *Client) CommitFiles(ctx context.Context, repo, sha string) ([]string, error) {
	next := c.repoURL(repo, "commits", sha) + "?per_page=100"
	var out []string
	for next != "" {
		var page commitDetail
		resp, err := c.do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, f := range page.Files {
			out = append(out, f.Filename)
		}
		next = nextLink(resp)
	}
	return out, nil
}

// Dispatch 触发目标仓库的 repository_dispatch（03 §2.6 的接收端）。
//
// 这也是为什么 store 侧的 PAT 需要 forge 仓库的 **Contents: write** ——
// repository_dispatch 要的是目标仓库的 contents 权限，不是 actions 权限（03 §6.1）。
func (c *Client) Dispatch(ctx context.Context, repo, eventType string, payload map[string]any) error {
	u := c.repoURL(repo, "dispatches")
	body := map[string]any{"event_type": eventType}
	if payload != nil {
		body["client_payload"] = payload
	}
	_, err := c.do(ctx, http.MethodPost, u, body, nil)
	return err
}

// ---- 工具 -------------------------------------------------------------------

// repoURL 拼 `{base}/repos/{owner/name}/{segments...}`。
func (c *Client) repoURL(repo string, segments ...string) string {
	parts := make([]string, 0, len(segments)+2)
	parts = append(parts, "repos", repo)
	parts = append(parts, segments...)
	// repo 里的 `/` 必须留在路径里（它是 owner/name 的分隔），所以逐个转义其余段。
	escaped := make([]string, 0, len(parts))
	for i, p := range parts {
		if i == 1 {
			escaped = append(escaped, p) // owner/name 原样
			continue
		}
		escaped = append(escaped, url.PathEscape(p))
	}
	return c.baseURL.String() + "/" + strings.Join(escaped, "/")
}

// nextLink 从 Link 头里取 rel="next"。
//
// 手写而不是引库：格式就是 `<url>; rel="next", <url>; rel="prev"`，
// 而"翻页翻丢了"的后果是**静默漏数据**（对账少看到一个上游 Release），
// 所以这段逻辑配了单测。
func nextLink(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	for _, part := range strings.Split(resp.Header.Get("Link"), ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		u := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(u, "<") || !strings.HasSuffix(u, ">") {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(u, "<>")
			}
		}
	}
	return ""
}
