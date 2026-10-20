// Package markup 提供公告正文的最小 Markdown → HTML 渲染（D7a；R-D7-04）。
//
// 【为什么新建包而不是放进 internal/settings】settings 的 sanitize.go 处理的是
// 「HTML 入站面的标签白名单」（站点页脚/关于页），语义是「解析已有 HTML 并
// 剥离」。公告需要的是「不解析 HTML、从 Markdown 生成标签」——两者白名单
// 不同（R-04-9 明文要求不得混用），放进同一文件会让后来者误以为可以复用。
//
// 【为什么是净室最小实现而不是引入第三方解析器】公告正文的语法集合是冻结的
// 小集合（段落/标题 h2–h4/粗斜体/行内代码/代码块/链接/列表/引用）。引入通用
// Markdown 解析器意味着把「原始 HTML 透传、自动链接、图片、表格」等未冻结的
// 能力带进渲染面，每一类都是新的 XSS 入口；自有实现能把「不认识的语法一律
// 当纯文本转义」作为默认行为。
//
// 【安全模型】输入永远不可信（管理员账号也可能被钓鱼），因此：
//   - 原始 HTML **不解析**：`<` 一律转义成 `&lt;`，`<script>`、`<img onerror>`
//     等只会以文本出现，绝不会生成标签或事件属性；
//   - 链接只接受 http/https 绝对 URL；`javascript:`/`data:`/`vbscript:`/相对
//     路径一律**丢弃 href**、保留链接文本（写库不失败，避免历史正文突然
//     422 不可编辑）；
//   - 所有生成的属性值经过属性转义，外链强制 rel="noopener noreferrer"。
//
// 这是一个**渲染器**，不是消毒器：不在输出后再做正则清洗——那类后处理正是
// 属性引号/大小写/畸形标签绕过的高发区。
package markup

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrInvalidMarkdown 表示正文不是合法 UTF-8（无法安全渲染）。
var ErrInvalidMarkdown = errors.New("markup: markdown content is not valid UTF-8")

// headingPattern 只匹配 h2–h4（R-D7-04 冻结的标题级别）。`#` 与 `#####`
// 不匹配，按普通段落文本转义输出——不静默提升/降低标题层级。
var headingPattern = regexp.MustCompile(`^(#{2,4})[ \t]+(.*)$`)

// orderedItemPattern 匹配有序列表项（`1.` 或 `1)`，数字不参与编号输出，
// 由 <ol> 自动编号）。
var orderedItemPattern = regexp.MustCompile(`^[0-9]+[.)][ \t]+(.*)$`)

// MarkdownToHTML 把公告正文渲染为净化后的 HTML 片段。
//
// 返回错误仅限「无法安全渲染」的输入（非法 UTF-8）；含原始 HTML 或危险链接
// 的正文**不报错**，而是降级为纯文本/无 href 链接。这样已发布的历史内容在
// 渲染器升级后仍可展示，不会让列表接口整体 500。
func MarkdownToHTML(src string) (string, error) {
	if !utf8.ValidString(src) {
		return "", ErrInvalidMarkdown
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var out strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			i++
			continue
		}

		// 围栏代码块优先：内部内容不参与任何行内解析。
		if strings.HasPrefix(trimmed, "```") {
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			// 未闭合的围栏按「直到文末」处理：比把剩余正文全部当代码吞掉更
			// 可预测的做法是照常关闭，绝不把未转义内容漏出去。
			if i < len(lines) {
				i++ // 跳过关闭围栏
			}
			out.WriteString("<pre><code>")
			out.WriteString(escapeHTML(strings.Join(code, "\n")))
			out.WriteString("</code></pre>")
			continue
		}

		if m := headingPattern.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			out.WriteString("<h")
			out.WriteByte(byte('0' + level))
			out.WriteString(">")
			out.WriteString(renderInline(m[2]))
			out.WriteString("</h")
			out.WriteByte(byte('0' + level))
			out.WriteString(">")
			i++
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			var quoted []string
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(t, ">") {
					break
				}
				quoted = append(quoted, strings.TrimPrefix(strings.TrimPrefix(t, ">"), " "))
				i++
			}
			out.WriteString("<blockquote><p>")
			out.WriteString(renderLines(quoted))
			out.WriteString("</p></blockquote>")
			continue
		}

		if isUnorderedItem(line) {
			out.WriteString("<ul>")
			for i < len(lines) && isUnorderedItem(lines[i]) {
				out.WriteString("<li>")
				out.WriteString(renderInline(strings.TrimSpace(lines[i])[2:]))
				out.WriteString("</li>")
				i++
			}
			out.WriteString("</ul>")
			continue
		}

		if m := orderedItemPattern.FindStringSubmatch(line); m != nil {
			out.WriteString("<ol>")
			for i < len(lines) {
				item := orderedItemPattern.FindStringSubmatch(lines[i])
				if item == nil {
					break
				}
				out.WriteString("<li>")
				out.WriteString(renderInline(item[1]))
				out.WriteString("</li>")
				i++
			}
			out.WriteString("</ol>")
			continue
		}

		// 段落：连续的非空、非块起始行合并，行内换行保留为 <br>。
		var para []string
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" || startsBlock(lines[i]) {
				break
			}
			para = append(para, lines[i])
			i++
		}
		out.WriteString("<p>")
		out.WriteString(renderLines(para))
		out.WriteString("</p>")
	}
	return out.String(), nil
}

// renderLines 渲染一组「同一段落内」的行：逐行做行内解析，再用 <br>
// 保留源文本的换行（Markdown 的 hard break 在公告编辑器里被当作换行使用）。
func renderLines(lines []string) string {
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered = append(rendered, renderInline(line))
	}
	return strings.Join(rendered, "<br>")
}

// startsBlock 判断一行是否开启新块（段落收集的终止条件）。
func startsBlock(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") ||
		strings.HasPrefix(trimmed, ">") ||
		isUnorderedItem(line) ||
		headingPattern.MatchString(line) ||
		orderedItemPattern.MatchString(line)
}

func isUnorderedItem(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 2 {
		return false
	}
	switch trimmed[0] {
	case '-', '*', '+':
		return trimmed[1] == ' ' || trimmed[1] == '\t'
	}
	return false
}

// renderInline 处理行内语法。所有文本片段都经过 escapeHTML；原始 HTML
// 因此永远以文本出现，不会变成标签。
func renderInline(s string) string {
	var out strings.Builder
	writeInline(&out, s, 0)
	return out.String()
}

// maxInlineDepth 限制嵌套（链接文本/粗体内的递归）深度，防止畸形输入
// 造成栈耗尽。超深内容按纯文本转义输出。
const maxInlineDepth = 8

func writeInline(out *strings.Builder, s string, depth int) {
	if depth > maxInlineDepth {
		out.WriteString(escapeHTML(s))
		return
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				out.WriteString("<code>")
				out.WriteString(escapeHTML(s[i+1 : i+1+j]))
				out.WriteString("</code>")
				i += j + 2
				continue
			}
		case '[':
			if text, url, next, ok := parseLink(s, i); ok {
				if url != "" {
					out.WriteString(`<a href="`)
					out.WriteString(escapeAttr(url))
					out.WriteString(`" rel="noopener noreferrer">`)
					writeInline(out, text, depth+1)
					out.WriteString("</a>")
				} else {
					// 危险/不支持的链接：丢弃 href，只保留链接文本。
					writeInline(out, text, depth+1)
				}
				i = next
				continue
			}
		case '*', '_':
			if next, ok := writeEmphasis(out, s, i, depth); ok {
				i = next
				continue
			}
		}
		// 普通文本：一直吃到下一个行内特殊字符，整段转义后写出。
		// 不能逐字节写：多字节 UTF-8 会被拆坏。
		next := nextInlineSpecial(s, i)
		if next == i {
			// 当前字节是特殊字符但没有匹配到任何语法（如孤立的 `*`）：
			// 必须前进一格，否则外层循环原地打转。
			out.WriteString(escapeHTML(s[i : i+1]))
			i++
			continue
		}
		out.WriteString(escapeHTML(s[i:next]))
		i = next
	}
}

// writeEmphasis 识别 **x**/__x__（strong）与 *x*/_x_（em）。返回处理后
// 的下标；不匹配时返回 ok=false，调用方按文本处理。
func writeEmphasis(out *strings.Builder, s string, i, depth int) (int, bool) {
	c := s[i]
	if i+1 < len(s) && s[i+1] == c {
		if j := strings.Index(s[i+2:], s[i:i+2]); j > 0 {
			out.WriteString("<strong>")
			writeInline(out, s[i+2:i+2+j], depth+1)
			out.WriteString("</strong>")
			return i + 2 + j + 2, true
		}
	}
	if j := strings.IndexByte(s[i+1:], c); j > 0 {
		out.WriteString("<em>")
		writeInline(out, s[i+1:i+1+j], depth+1)
		out.WriteString("</em>")
		return i + 1 + j + 1, true
	}
	return i, false
}

// parseLink 解析 [text](url)（无可选 title）。url 不安全时返回空串但
// ok=true——调用方保留文本、丢弃链接。
func parseLink(s string, i int) (text, url string, next int, ok bool) {
	closeBracket := strings.IndexByte(s[i+1:], ']')
	if closeBracket < 0 {
		return "", "", 0, false
	}
	afterBracket := i + 1 + closeBracket + 1
	if afterBracket >= len(s) || s[afterBracket] != '(' {
		return "", "", 0, false
	}
	// 括号按深度配对：`[x](javascript:alert(1))` 的 URL 里含括号，取第一个
	// `)` 会把 `alert(1` 截成 URL、把尾部 `)` 漏成正文。深度扫描找到真正
	// 的收尾括号，整段 URL 交给 safeLinkURL 判定。
	closeParen := -1
	depth := 0
	for j := afterBracket + 1; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				closeParen = j
			} else {
				depth--
			}
		}
		if closeParen >= 0 {
			break
		}
	}
	if closeParen < 0 {
		return "", "", 0, false
	}
	text = s[i+1 : i+1+closeBracket]
	url = strings.TrimSpace(s[afterBracket+1 : closeParen])
	next = closeParen + 1
	if !safeLinkURL(url) {
		return text, "", next, true
	}
	return text, url, next, true
}

// safeLinkURL 只接受 http/https 绝对 URL（R-D7-04 冻结口径）。其余
// 协议（javascript:/data:/vbscript:/file:）与相对路径一律拒绝。
//
// 额外的字符级检查针对的是「协议前缀合法但属性值里带控制字符/引号」的变形：
// 属性转义已经处理引号，这里再拒绝控制字符，避免浏览器把换行当作分隔符。
func safeLinkURL(raw string) bool {
	if raw == "" {
		return false
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	for i := 0; i < len(lower); i++ {
		switch lower[i] {
		case '\n', '\r', '\t', ' ':
			return false
		}
		if lower[i] < 0x20 || lower[i] == 0x7f {
			return false
		}
	}
	// 必须有主机名：`https://` 或 `https:///path` 不是可用外链。
	rest := lower[strings.Index(lower, "://")+3:]
	host := rest
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	return host != ""
}

// nextInlineSpecial 返回下一个行内特殊字符的下标（无则返回 len(s)）。
func nextInlineSpecial(s string, i int) int {
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '`', '[', '*', '_':
			return j
		}
	}
	return len(s)
}

// PlainTextHTML 是渲染失败的降级出口：把整段正文当纯文本转义后包进段落。
// 设计验收要求「消毒后为空/解析失败降级纯文本展示」——返回空串会让公告
// 在弹层里彻底消失且无任何提示，纯文本至少保留信息。
func PlainTextHTML(src string) string {
	return "<p>" + escapeHTML(src) + "</p>"
}

func escapeHTML(s string) string {
	return htmlEscaper.Replace(s)
}

func escapeAttr(s string) string {
	return htmlEscaper.Replace(s)
}

// htmlEscaper 统一转义五类会改变 HTML 结构的字符。`"` 与 `'` 也转义，
// 让同一函数可安全用于属性值。
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&#34;",
	"'", "&#39;",
)
