package gh_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gh"
)

// uploadFile 造一个**真实文件**当上传的入参 —— 生产里就是 *os.File。
//
// 这里不能图省事用 strings.NewReader：http.NewRequest 只对
// *bytes.Buffer / *bytes.Reader / *strings.Reader 自动推断长度，那三种会被
// 自动补上 Content-Length，于是**即使 UploadAsset 不设长度测试也照样通过** ——
// 测试会把要钉的那一行完美绕过去。*os.File 不在名单里，正好与生产一致。
func uploadFile(t *testing.T, content string) (*os.File, int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.apk")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, fi.Size()
}

// testToken 用一个明显可识别的值，好让"token 有没有漏进错误信息"这类断言好写。
const testToken = "github_pat_11ABCDEFG_secret_value_do_not_log"

func newTestClient(t *testing.T, h http.HandlerFunc) (*gh.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := gh.New(gh.Config{
		Token:         testToken,
		BaseURL:       srv.URL,
		UploadBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("gh.New：%v", err)
	}
	return c, srv
}

// TestAuthHeaderAndNoTokenInURL 同时钉两件事：token 走了 header，且**没进 URL**。
//
// 后者是 03 §4.5 规则 8 的核心（"token 绝不拼进 URL"）—— URL 会进日志、进 http 错误、
// 进 Referer，而 header 不会。
func TestAuthHeaderAndNoTokenInURL(t *testing.T) {
	var gotAuth, gotURL string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.String()
		io.WriteString(w, `{"id":1,"tag_name":"_incoming"}`)
	})

	if _, err := c.GetRelease(context.Background(), "market-of-labs/store", "_incoming"); err != nil {
		t.Fatalf("GetRelease：%v", err)
	}
	if want := "Bearer " + testToken; gotAuth != want {
		t.Errorf("Authorization = %q，期望 %q", gotAuth, want)
	}
	if strings.Contains(gotURL, testToken) {
		t.Errorf("token 出现在 URL 里了：%q", gotURL)
	}
	if strings.Contains(gotURL, "access_token") {
		t.Errorf("URL 里出现了 access_token 参数：%q", gotURL)
	}
}

func TestAPIVersionAndUserAgent(t *testing.T) {
	var gotVer, gotUA string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("X-GitHub-Api-Version")
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, `{}`)
	})
	if _, err := c.GetIssue(context.Background(), "o/r", 1); err != nil {
		t.Fatalf("GetIssue：%v", err)
	}
	// 写死版本而不是 latest：GitHub 改版时不该悄悄换掉我们依赖的字段形状。
	if gotVer != gh.APIVersion {
		t.Errorf("X-GitHub-Api-Version = %q，期望 %q", gotVer, gh.APIVersion)
	}
	if gotUA == "" {
		t.Error("User-Agent 为空 —— GitHub 会拒绝没有 UA 的请求")
	}
}

// TestNotFoundIsSentinel 钉住 404 的错误语义。
//
// 幂等逻辑全靠它：`_incoming` 的 Release 不存在是**正常**的（该建），
// 而权限不足/网络故障必须失败。把两者混成同一个 error，会让代码变成"每次重建 Release"。
func TestNotFoundIsSentinel(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.GetRelease(context.Background(), "o/r", "nope")
	if err == nil {
		t.Fatal("404 应当返回错误")
	}
	if !errors.Is(err, gh.ErrNotFound) {
		t.Errorf("404 应当能被 errors.Is(err, gh.ErrNotFound) 识别，得到：%v", err)
	}
}

func TestNonNotFoundErrorIsNotSentinel(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"Resource not accessible by personal access token"}`)
	})

	_, err := c.GetRelease(context.Background(), "o/r", "x")
	if err == nil {
		t.Fatal("403 应当返回错误")
	}
	// 403 绝不能被误判成"不存在" —— 那会让 forge 以为 Release 该建，
	// 于是拿权限不足去反复重试建 Release。
	if errors.Is(err, gh.ErrNotFound) {
		t.Error("403 不该被当成 404")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误里应当带状态码：%v", err)
	}
}

// TestErrorsNeverLeakToken 是 03 §4.5 规则 8/9 的回归测试。
//
// 公有仓库的 Actions 日志**任何登录用户都能读**（03 §4.2 末），所以"错误信息里
// 不能出现 token"不是洁癖而是硬要求。这里覆盖三条最可能泄露的路径：
// 连接失败、HTTP 错误、以及响应体里回显。
func TestErrorsNeverLeakToken(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		call    func(*gh.Client) error
	}{
		{
			name: "HTTP 403 且响应体回显了 token",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"message":"bad token %s"}`, testToken)
			},
			call: func(c *gh.Client) error {
				_, err := c.GetRelease(context.Background(), "o/r", "t")
				return err
			},
		},
		{
			name: "响应不是合法 JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `not json at all`)
			},
			call: func(c *gh.Client) error {
				_, err := c.GetRelease(context.Background(), "o/r", "t")
				return err
			},
		},
		{
			name: "下载 asset 时出错",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "boom")
			},
			call: func(c *gh.Client) error {
				_, _, err := c.DownloadAsset(context.Background(), "o/r", 1)
				return err
			},
		},
		{
			name: "上传 asset 时出错",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, "too big")
			},
			call: func(c *gh.Client) error {
				_, err := c.UploadAsset(context.Background(), "o/r", 1, "a.apk", strings.NewReader("x"), 1)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.handler)
			err := tc.call(c)
			if err == nil {
				t.Fatal("该报错却成功了")
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("错误信息里泄露了 token：%v", err)
			}
		})
	}
}

// TestRedactsURLQuery 钉住"错误里的 URL 不带 query"。
// 万一将来有人图省事把凭据放进 query，这一条会挡住它进日志。
func TestRedactsURLQuery(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})

	// 直接构造一个带 query 的失败：换个不存在的路径 + 服务已关闭。
	srv.Close()
	_, err := c.ListReleases(context.Background(), "o/r")
	if err == nil {
		t.Skip("连接失败没能复现")
	}
	if strings.Contains(err.Error(), "per_page") {
		t.Errorf("错误信息里带上了 query：%v", err)
	}
}

// TestPaginationFollowsLink 钉住翻页。
//
// 翻页翻丢的后果是**静默漏数据** —— 对账会少看到一个上游 Release 或少算一批 asset，
// 而且不会有任何报错。所以这条必须有测试。
func TestPaginationFollowsLink(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link",
				fmt.Sprintf(`<%s/repos/o/r/releases?per_page=100&page=2>; rel="next", <%s/repos/o/r/releases?per_page=100&page=1>; rel="prev"`, srvURL, srvURL))
			io.WriteString(w, `[{"id":1,"tag_name":"a"}]`)
		case "2":
			// 最后一页不给 Link：翻页必须在这里停住，否则会无限循环。
			io.WriteString(w, `[{"id":2,"tag_name":"b"}]`)
		default:
			t.Errorf("请求了意料之外的 page=%q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	c, err := gh.New(gh.Config{BaseURL: srv.URL, UploadBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ListReleases(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("ListReleases：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("翻页后拿到 %d 条，期望 2 条：%+v", len(got), got)
	}
	if got[0].TagName != "a" || got[1].TagName != "b" {
		t.Errorf("顺序不对：%+v", got)
	}
}

func TestPaginationSendsPerPage100(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `[]`)
	})
	if _, err := c.ListReleases(context.Background(), "o/r"); err != nil {
		t.Fatal(err)
	}
	// 默认 30 会让一个发版频繁的上游多花三四倍请求，而匿名限额只有 60/小时。
	if !strings.Contains(gotQuery, "per_page=100") {
		t.Errorf("首个请求的 query 是 %q，应当带 per_page=100", gotQuery)
	}
}

// TestUnpublishUsesDraftNotDelete 钉住 03 §4.5 规则 6。
//
// 删 Release 会让 tag 消失；若曾开启 Immutable Releases 则**永久烧毁该 tag**，
// 而 tag = appId 是不可重建的安装身份。所以清场只能用 draft:true。
//
// 同时钉住 tag_name 必须跟着一起发：不带它的 PATCH draft 会让 GitHub 把 tag 名换成
// `untagged-<sha>` 占位名，队列从此认不出来 —— 见 gh.Unpublish 的说明与那两次实测。
func TestUnpublishUsesDraftNotDelete(t *testing.T) {
	var method string
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"id":7,"draft":true}`)
	})

	rel, err := c.Unpublish(context.Background(), "market-of-labs/store", 7, "_incoming")
	if err != nil {
		t.Fatalf("Unpublish：%v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("方法 = %s，必须是 PATCH（DELETE 会烧毁 tag）", method)
	}
	if !strings.Contains(body, `"draft":true`) {
		t.Errorf("请求体里没有 draft:true：%s", body)
	}
	// 少了这一条，GitHub 就会把 tag 名换成 `untagged-*` —— 队列当场报废。
	if !strings.Contains(body, `"tag_name":"_incoming"`) {
		t.Errorf("请求体里没有 tag_name（会让 GitHub 把 tag 名换成占位名）：%s", body)
	}
	// 规则 4：每次 draft→published 都会重打 published_at，make_latest 必须显式 false。
	if !strings.Contains(body, `"make_latest":"false"`) {
		t.Errorf("请求体里没有 make_latest:false（规则 4）：%s", body)
	}
	if !rel.Draft {
		t.Error("返回的 Release 应当是 draft")
	}
}

// `_incoming` 的 tag 引用必须由清场删掉：`release` 事件跑的是**这个引用所指提交**上的
// workflow，而引用一旦建立就不再移动，于是默认分支上改的触发器对它永远不可见（实测：
// 09-12 建的引用，09-16 的发布还在用；症状是「Publish 了却没反应」且 Actions 干干净净）。
// 删掉之后 Publish 会在当时的默认分支 HEAD 上重建它。
func TestDeleteTagRefTargetsRefsEndpoint(t *testing.T) {
	var method, gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.DeleteTagRef(context.Background(), "market-of-labs/store", "_incoming"); err != nil {
		t.Fatalf("DeleteTagRef：%v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("方法 = %s，必须是 DELETE", method)
	}
	// 斜杠必须留在路径里（转义成 %2F 就不是 GitHub 认的那个端点了）。
	if gotPath != "/repos/market-of-labs/store/git/refs/tags/_incoming" {
		t.Errorf("路径不对：%q", gotPath)
	}
}

// 队列常驻 draft 时压根没有这个引用，404 是常态而不是失败 —— 清场不该因此报错。
func TestDeleteTagRefToleratesNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	})
	if err := c.DeleteTagRef(context.Background(), "market-of-labs/store", "_incoming"); err != nil {
		t.Errorf("404 应当被吞掉，却报了：%v", err)
	}
}

func TestRepoSlugStaysInPath(t *testing.T) {
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"id":1}`)
	})
	if _, err := c.GetRelease(context.Background(), "market-of-labs/store", "_incoming"); err != nil {
		t.Fatal(err)
	}
	// owner/name 里的斜杠必须留在路径里（它是分隔符，不是要转义的数据）。
	if !strings.Contains(gotPath, "/repos/market-of-labs/store/releases/tags/_incoming") {
		t.Errorf("路径不对：%q", gotPath)
	}
}

// 上传必须**自报长度**。uploads.github.com 拒收 chunked（400 "Bad Content-Length"），
// 而 httptest 服务端对 chunked 是照单全收的 —— 所以这一条只有把 ContentLength 本身钉住
// 才有意义：只看"服务端收到了完整 body"是钉不住的，那正是漏掉这个 bug 的原因。
//
// 曾经用 io.Pipe 拼 multipart 流式发送（长度不可知 → chunked），于是一个 asset 都没传上去，
// 而 Release 建得好好的、里面永远空着。复现方式：把 ContentLength 那行删掉，
// 这里会读到 -1（分块传输），真实环境则直接 400。
func TestUploadSendsRawBodyWithContentLength(t *testing.T) {
	const content = "APKDATA"
	var gotName, gotCT string
	var gotCL, gotLen int64
	var gotBody string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotName = r.URL.Query().Get("name")
		gotCT = r.Header.Get("Content-Type")
		gotCL = r.ContentLength
		b, _ := io.ReadAll(r.Body)
		gotLen, gotBody = int64(len(b)), string(b)
		io.WriteString(w, `{"id":9,"name":"com.foo-1.0-universal.apk"}`)
	})

	f, size := uploadFile(t, content)
	a, err := c.UploadAsset(context.Background(), "market-of-labs/store", 7,
		"com.foo-1.0-universal.apk", f, size)
	if err != nil {
		t.Fatalf("UploadAsset：%v", err)
	}
	if gotName != "com.foo-1.0-universal.apk" {
		t.Errorf("?name= 是 %q", gotName)
	}
	if gotCL != int64(len(content)) {
		t.Errorf("Content-Length = %d（-1 表示走成了 chunked，GitHub 会回 400）：want %d",
			gotCL, len(content))
	}
	if gotLen != int64(len(content)) || gotBody != content {
		t.Errorf("body 应当就是文件本身：CL=%d body=%q", gotLen, gotBody)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if a.ID != 9 {
		t.Errorf("返回的 asset id = %d", a.ID)
	}
}

// 长度报错了必须**报错**，而不是悄悄传半截 —— 半个 APK 比没有更糟：
// 客户端会当成一个能用的版本下下来。Go 的传输层在这里挡得住。
func TestUploadRejectsShortBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"id":9}`)
	})
	f, _ := uploadFile(t, "短")
	_, err := c.UploadAsset(context.Background(), "market-of-labs/store", 7,
		"com.foo-1.0-universal.apk", f, 999)
	if err == nil {
		t.Fatal("声明的长度与 body 不符时应当报错，而不是传出去")
	}
}

func TestDispatchPayloadShape(t *testing.T) {
	var gotPath, gotBody string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})

	// 事件类型现在是 per-功能 的一个词（`intake-incoming` / `source-change` / `reconcile`），
	// 不再是那个什么事件都塞进去的 `store-event` —— 这里用一个真实取值，免得 fixture
	// 里留着一个 grep 扫不干净的旧名字。
	err := c.Dispatch(context.Background(), "market-of-labs/forge", "intake-incoming",
		map[string]any{"issue": 0})
	if err != nil {
		t.Fatalf("Dispatch：%v", err)
	}
	if gotPath != "/repos/market-of-labs/forge/dispatches" {
		t.Errorf("路径 = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"event_type":"intake-incoming"`) {
		t.Errorf("缺 event_type：%s", gotBody)
	}
	if !strings.Contains(gotBody, `"client_payload"`) {
		t.Errorf("缺 client_payload：%s", gotBody)
	}
}

func TestDownloadAssetAsksForOctetStream(t *testing.T) {
	var gotAccept string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		io.WriteString(w, "BINARY")
	})

	rc, _, err := c.DownloadAsset(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatalf("DownloadAsset：%v", err)
	}
	defer rc.Close()
	// 不显式要 octet-stream 的话会拿到 JSON 元数据，而不是文件内容。
	if gotAccept != "application/octet-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "BINARY" {
		t.Errorf("内容 = %q", b)
	}
}

// TestReadme 钉住三件事：端点路径、**带换行的 base64** 解得开、没有 README 时是空串而非错误。
//
// 三条都是真会踩的：路径写错是 404（被当成"上游没写 README"而静默），base64 里每 60 个
// 字符一个 \n（用 StdEncoding.DecodeString 会当场报错），而 404 若当成错误冒泡上去，
// 一个没有 README 的上游会在每轮对账里刷一条 WARN。
func TestReadme(t *testing.T) {
	var gotPath string
	const md = "# 示例项目\n\n一段说明。\n"
	// 这是 GitHub 真实返回的形状：base64，且每 60 个字符插一个换行。
	var buf strings.Builder
	enc := base64.StdEncoding.EncodeToString([]byte(md))
	for i := 0; i < len(enc); i += 60 {
		end := i + 60
		if end > len(enc) {
			end = len(enc)
		}
		buf.WriteString(enc[i:end] + "\n")
	}

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(gotPath, "noreadme") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"name":"README.md","encoding":"base64","content":%q}`, buf.String())
	})

	got, err := c.Readme(context.Background(), "owner/up")
	if err != nil {
		t.Fatalf("Readme：%v", err)
	}
	if gotPath != "/repos/owner/up/readme" {
		t.Errorf("路径 = %q", gotPath)
	}
	if got != md {
		t.Errorf("正文 = %q，期望 %q", got, md)
	}

	// 上游没有 README：空串 + nil（调用方什么都不做），而**不是**一个错误。
	empty, err := c.Readme(context.Background(), "owner/noreadme")
	if err != nil {
		t.Fatalf("没有 README 不该报错：%v", err)
	}
	if empty != "" {
		t.Errorf("没有 README 时应为空串，得到 %q", empty)
	}
}
