package markup

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// allowedTags 是渲染器允许输出的全部标签（R-D7-04 的最小集）。XSS 矩阵的
// 每条用例都先过这个断言：任何未登记的标签（script/img/iframe…）出现即失败。
var allowedTags = map[string]bool{
	"a": true, "p": true, "br": true, "strong": true, "em": true,
	"h2": true, "h3": true, "h4": true, "ul": true, "ol": true,
	"li": true, "code": true, "pre": true, "blockquote": true,
}

var tagPattern = regexp.MustCompile(`</?([a-zA-Z][a-zA-Z0-9]*)[^>]*>`)

func assertOnlyAllowedTags(t *testing.T, html string) {
	t.Helper()
	for _, m := range tagPattern.FindAllStringSubmatch(html, -1) {
		if !allowedTags[strings.ToLower(m[1])] {
			t.Fatalf("输出含白名单外标签 <%s>：%q", m[1], html)
		}
	}
}

func render(t *testing.T, src string) string {
	t.Helper()
	out, err := MarkdownToHTML(src)
	if err != nil {
		t.Fatalf("MarkdownToHTML(%q) error: %v", src, err)
	}
	assertOnlyAllowedTags(t, out)
	return out
}

// TestMarkdownRendersBasicSyntax 覆盖冻结的语法集合（段落/标题 h2–h4/
// 粗斜体/行内代码/代码块/链接/列表/引用）。
func TestMarkdownRendersBasicSyntax(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"paragraph", "hello world", []string{"<p>hello world</p>"}},
		{"paragraph line break", "line one\nline two", []string{"<p>line one<br>line two</p>"}},
		{"h2", "## Heading", []string{"<h2>Heading</h2>"}},
		{"h3", "### Heading", []string{"<h3>Heading</h3>"}},
		{"h4", "#### Heading", []string{"<h4>Heading</h4>"}},
		{"bold", "a **strong** b", []string{"<strong>strong</strong>"}},
		{"bold underscore", "__strong__", []string{"<strong>strong</strong>"}},
		{"italic", "a *em* b", []string{"<em>em</em>"}},
		{"italic underscore", "_em_", []string{"<em>em</em>"}},
		{"inline code", "run `go test` now", []string{"<code>go test</code>"}},
		{"fenced code", "```\nx := 1\n```", []string{"<pre><code>x := 1</code></pre>"}},
		{"unordered list", "- one\n- two", []string{"<ul>", "<li>one</li>", "<li>two</li>"}},
		{"ordered list", "1. one\n2. two", []string{"<ol>", "<li>one</li>", "<li>two</li>"}},
		{"blockquote", "> quoted", []string{"<blockquote><p>quoted</p></blockquote>"}},
		{"link", "[docs](https://example.com/a?b=1&c=2)",
			[]string{`<a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer">docs</a>`}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			out := render(t, c.src)
			for _, fragment := range c.want {
				if !strings.Contains(out, fragment) {
					t.Fatalf("输出缺少 %q：%q", fragment, out)
				}
			}
		})
	}
}

// TestMarkdownDoesNotRenderH1 钉死 h2–h4 的级别边界：`#` 与 `#####` 不是
// 标题，必须按普通文本展示（不静默提升/降低层级）。
func TestMarkdownDoesNotRenderH1(t *testing.T) {
	for _, src := range []string{"# h1", "##### h5", "###### h6"} {
		out := render(t, src)
		if strings.Contains(out, "<h1>") || strings.Contains(out, "<h5>") || strings.Contains(out, "<h6>") {
			t.Fatalf("%q 不应生成标题：%q", src, out)
		}
	}
}

// TestMarkdownXSSMatrix 是 R-D7-04 要求的 XSS 矩阵：原始 HTML 一律以文本
// 出现，危险链接丢弃 href，白名单外标签被剥离（这里是「不生成」）。
func TestMarkdownXSSMatrix(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		mustContain []string
		mustNot     []string
	}{
		{
			name: "script tag",
			src:  `<script>alert(1)</script>`,
			// 原文被转义成文本：证明「不解析」而不是「删掉后无法审计」。
			mustContain: []string{"&lt;script&gt;"},
			mustNot:     []string{"<script", "</script"},
		},
		{
			name:        "img onerror",
			src:         `<img src=x onerror="alert(1)">`,
			mustContain: []string{"&lt;img"},
			mustNot:     []string{"<img", "<img ", "onerror=\"alert"},
		},
		{
			name:        "raw b tag",
			src:         `<b>bold</b>`,
			mustContain: []string{"&lt;b&gt;"},
			mustNot:     []string{"<b>"},
		},
		{
			name:        "javascript link",
			src:         `[click](javascript:alert(1))`,
			mustContain: []string{"<p>click</p>"},
			mustNot:     []string{"javascript:", "href="},
		},
		{
			name:        "data link",
			src:         `[x](data:text/html;base64,PHNjcmlwdD4=)`,
			mustContain: []string{"<p>x</p>"},
			mustNot:     []string{"data:", "href="},
		},
		{
			name:        "vbscript link",
			src:         `[x](vbscript:msgbox(1))`,
			mustContain: []string{"<p>x</p>"},
			mustNot:     []string{"vbscript:", "href="},
		},
		{
			name:        "relative link rejected",
			src:         `[x](/admin)`,
			mustContain: []string{"<p>x</p>"},
			mustNot:     []string{"href="},
		},
		{
			name:        "attribute injection via quote",
			src:         `[x](https://evil.example/"onmouseover="alert(1))`,
			mustContain: []string{"&#34;onmouseover=&#34;"},
			mustNot:     []string{`" onmouseover="`},
		},
		{
			name:        "html inside code fence is escaped",
			src:         "```\n<script>alert(1)</script>\n```",
			mustContain: []string{"&lt;script&gt;"},
			mustNot:     []string{"<script"},
		},
		{
			name:        "html inside inline code is escaped",
			src:         "`<script>alert(1)</script>`",
			mustContain: []string{"<code>&lt;script&gt;"},
			mustNot:     []string{"<script"},
		},
		{
			name:        "control characters in link rejected",
			src:         "[x](https://a.example/\npath)",
			mustContain: []string{"<p>"},
			mustNot:     []string{"href="},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			out := render(t, c.src)
			for _, fragment := range c.mustContain {
				if !strings.Contains(out, fragment) {
					t.Fatalf("输出缺少 %q：%q", fragment, out)
				}
			}
			for _, fragment := range c.mustNot {
				if strings.Contains(out, fragment) {
					t.Fatalf("输出不应包含 %q：%q", fragment, out)
				}
			}
		})
	}
}

// TestMarkdownExternalLinkRel 验证外链强制 rel 且 href 经过属性转义。
func TestMarkdownExternalLinkRel(t *testing.T) {
	out := render(t, `[ex](https://example.com/?q="x")`)
	if !strings.Contains(out, `rel="noopener noreferrer"`) {
		t.Fatalf("外链缺少 rel：%q", out)
	}
	if !strings.Contains(out, "&#34;x&#34;") {
		t.Fatalf("href 未属性转义：%q", out)
	}
}

// TestMarkdownRejectsInvalidUTF8：无法安全渲染的输入返回错误（handler 422）。
func TestMarkdownRejectsInvalidUTF8(t *testing.T) {
	if _, err := MarkdownToHTML("\xff\xfe"); !errors.Is(err, ErrInvalidMarkdown) {
		t.Fatalf("非法 UTF-8 err = %v，want ErrInvalidMarkdown", err)
	}
}

// TestMarkdownUnclosedFence 钉死未闭合围栏的降级：剩余内容按代码转义并
// 正常闭合，绝不把未转义正文漏到输出里。
func TestMarkdownUnclosedFence(t *testing.T) {
	out := render(t, "```\n<script>")
	if strings.Contains(out, "<script") {
		t.Fatalf("未闭合围栏泄露原始标签：%q", out)
	}
	if !strings.HasSuffix(out, "</code></pre>") {
		t.Fatalf("未闭合围栏未正常闭合：%q", out)
	}
}
