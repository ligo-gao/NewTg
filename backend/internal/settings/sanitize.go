package settings

// HTML 入站面的白名单消毒（modules-08 §3.1 / security-04 §2.5 / R-04-9）。
//
// 白名单冻结为 {a, p, br, strong, em}：脚本、样式、事件属性、图片等一律
// 剥离；<a> 只保留 http(s)/站内相对 href，并强制 rel="noopener noreferrer"。
// 这是服务端入库前的最后一道防线——前端编辑器不可信（TC-F-08-01-04）。

import (
	"html"
	"regexp"
	"strings"
)

// allowedHTMLTags 是 HTML 入站面的冻结白名单。Markdown 出站面的集合更大，
// 两者不得混用（R-04-9）。
var allowedHTMLTags = map[string]struct{}{
	"a": {}, "p": {}, "br": {}, "strong": {}, "em": {},
}

var hrefAttrPattern = regexp.MustCompile(`(?i)(?:^|\s)href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

// sanitizeHTML 把任意输入收敛到白名单标签集。
//
// 实现是保守的流式标签扫描而不是正则替换：正则很难同时正确处理
// 属性引号、大小写与畸形标签，任何一个遗漏都会变成 XSS 入口。
func sanitizeHTML(input string) string {
	if input == "" {
		return ""
	}
	var out strings.Builder
	for i := 0; i < len(input); {
		lt := strings.IndexByte(input[i:], '<')
		if lt < 0 {
			out.WriteString(input[i:])
			break
		}
		if lt > 0 {
			out.WriteString(input[i : i+lt])
			i += lt
		}
		end := strings.IndexByte(input[i:], '>')
		if end < 0 {
			// 没有闭合尖括号：按文本处理，避免把后续内容吞掉。
			out.WriteString("&lt;")
			i++
			continue
		}
		raw := input[i+1 : i+end]
		i += end + 1
		out.WriteString(sanitizeTag(raw))
	}
	return out.String()
}

func sanitizeTag(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "&lt;&gt;"
	}
	// 注释、DOCTYPE、处理指令一律丢弃。
	if strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "?") {
		return ""
	}
	closing := strings.HasPrefix(raw, "/")
	if closing {
		raw = strings.TrimSpace(raw[1:])
	}
	name, rest := splitTagName(raw)
	name = strings.ToLower(name)
	if _, ok := allowedHTMLTags[name]; !ok {
		return ""
	}
	if closing {
		if name == "br" {
			return ""
		}
		return "</" + name + ">"
	}
	switch name {
	case "br":
		return "<br>"
	case "a":
		return "<a" + anchorAttrs(rest) + ">"
	default:
		// p/strong/em 的属性全部丢弃（白名单只约束标签，不保留样式/事件）。
		return "<" + name + ">"
	}
}

func splitTagName(raw string) (name, rest string) {
	end := 0
	for end < len(raw) {
		c := raw[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' {
			break
		}
		end++
	}
	return raw[:end], raw[end:]
}

func anchorAttrs(rest string) string {
	attrs := ` rel="noopener noreferrer"`
	matches := hrefAttrPattern.FindStringSubmatch(rest)
	if matches == nil {
		return attrs
	}
	href := matches[1]
	if href == "" {
		href = matches[2]
	}
	if href == "" {
		href = matches[3]
	}
	href = strings.TrimSpace(html.UnescapeString(href))
	if !safeHref(href) {
		return attrs
	}
	return ` href="` + html.EscapeString(href) + `"` + attrs
}

// safeHref 只接受 http(s) 绝对 URL、站内相对路径与页内锚点；
// javascript:/data:/vbscript: 等一律拒绝（§3.1 验收）。
func safeHref(href string) bool {
	if href == "" {
		return false
	}
	lower := strings.ToLower(href)
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "vbscript:") {
		return false
	}
	if strings.HasPrefix(href, "#") {
		return true
	}
	if strings.HasPrefix(href, "/") {
		return !strings.HasPrefix(href, "//")
	}
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
