package settings

import (
	"strings"
	"testing"
)

// F4：HTML 入站面白名单消毒（{a,p,br,strong,em}，a 强制 rel）。
func TestSanitizeHTMLWhitelist(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		bad  []string
	}{
		{
			name: "script 剥离",
			in:   `<p>hi</p><script>alert(1)</script>`,
			want: []string{"<p>hi</p>", "alert(1)"},
			bad:  []string{"<script", "</script>"},
		},
		{
			name: "事件属性剥离",
			in:   `<img src=x onerror="alert(1)">ok`,
			want: []string{"ok"},
			bad:  []string{"<img", "onerror"},
		},
		{
			name: "javascript href 拒绝",
			in:   `<a href="javascript:alert(1)">x</a>`,
			want: []string{`<a rel="noopener noreferrer">x</a>`},
			bad:  []string{"javascript:"},
		},
		{
			name: "外链保留 href 并强制 rel",
			in:   `<a href="https://example.com/a?b=1&amp;c=2" target="_blank" onclick="x()">link</a>`,
			want: []string{`href="https://example.com/a?b=1&amp;c=2"`, `rel="noopener noreferrer"`, "link"},
			bad:  []string{"target=", "onclick"},
		},
		{
			name: "站内相对路径保留",
			in:   `<a href="/docs">docs</a>`,
			want: []string{`href="/docs"`, `rel="noopener noreferrer"`},
		},
		{
			name: "白名单标签属性剥离",
			in:   `<p class="x">a <strong id="s">b</strong><br/></p>`,
			want: []string{"<p>a <strong>b</strong><br></p>"},
			bad:  []string{"class=", "id="},
		},
		{
			name: "畸形标签按文本",
			in:   `a < b`,
			want: []string{"a &lt; b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHTML(tc.in)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("输出 %q 缺少 %q", got, want)
				}
			}
			for _, bad := range tc.bad {
				if strings.Contains(got, bad) {
					t.Fatalf("输出 %q 仍含危险片段 %q", got, bad)
				}
			}
		})
	}
}

func TestSanitizeHTMLOnWrite(t *testing.T) {
	setting, _ := Lookup("site.footer_html")
	value, err := normalizeRaw(setting, jsonRaw(`"<p onclick=\"x\">ok</p><script>bad()</script>"`))
	if err != nil {
		t.Fatalf("normalizeRaw: %v", err)
	}
	got := value.(string)
	if strings.Contains(got, "onclick") || strings.Contains(got, "<script") {
		t.Fatalf("写入值未消毒: %q", got)
	}
	if !strings.Contains(got, "<p>ok</p>") {
		t.Fatalf("白名单内容丢失: %q", got)
	}
}

func jsonRaw(s string) []byte { return []byte(s) }
