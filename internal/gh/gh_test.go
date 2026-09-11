package gh_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gh"
)

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
				_, err := c.UploadAsset(context.Background(), "o/r", 1, "a.apk", strings.NewReader("x"))
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
func TestUnpublishUsesDraftNotDelete(t *testing.T) {
	var method string
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"id":7,"draft":true}`)
	})

	rel, err := c.Unpublish(context.Background(), "market-of-labs/store", 7)
	if err != nil {
		t.Fatalf("Unpublish：%v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("方法 = %s，必须是 PATCH（DELETE 会烧毁 tag）", method)
	}
	if !strings.Contains(body, `"draft":true`) {
		t.Errorf("请求体里没有 draft:true：%s", body)
	}
	// 规则 4：每次 draft→published 都会重打 published_at，make_latest 必须显式 false。
	if !strings.Contains(body, `"make_latest":"false"`) {
		t.Errorf("请求体里没有 make_latest:false（规则 4）：%s", body)
	}
	if !rel.Draft {
		t.Error("返回的 Release 应当是 draft")
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

func TestUploadSendsMultipartWithFilename(t *testing.T) {
	var gotName, gotCT string
	var gotBody string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotName = r.URL.Query().Get("name")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"id":9,"name":"com.foo-1.0-universal.apk"}`)
	})

	a, err := c.UploadAsset(context.Background(), "market-of-labs/store", 7,
		"com.foo-1.0-universal.apk", strings.NewReader("APKDATA"))
	if err != nil {
		t.Fatalf("UploadAsset：%v", err)
	}
	if gotName != "com.foo-1.0-universal.apk" {
		t.Errorf("?name= 是 %q", gotName)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("Content-Type = %q", gotCT)
	}
	// 文件名既在 query 里也在 part 的 filename 里 —— 两者不一致时 GitHub 以 query 为准，
	// 但只发 query 会让某些代理/日志看不出这是在传什么。
	if !strings.Contains(gotBody, `filename="com.foo-1.0-universal.apk"`) {
		t.Errorf("multipart 里没有 filename：%s", gotBody)
	}
	if !strings.Contains(gotBody, "APKDATA") {
		t.Errorf("multipart 里没有内容：%s", gotBody)
	}
	if a.ID != 9 {
		t.Errorf("返回的 asset id = %d", a.ID)
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

	err := c.Dispatch(context.Background(), "market-of-labs/forge", "store-event",
		map[string]any{"event": "release", "issue": 0})
	if err != nil {
		t.Fatalf("Dispatch：%v", err)
	}
	if gotPath != "/repos/market-of-labs/forge/dispatches" {
		t.Errorf("路径 = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"event_type":"store-event"`) {
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
