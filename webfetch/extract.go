package webfetch

import (
	"io"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// extractHTML turns a page into Markdown-ish plain text: headings, lists,
// tables, code blocks and links survive; scripts, styles, navigation,
// footers, forms and hidden elements don't. When the page marks its main
// content (<main>, role=main, <article>), only that is read.
func extractHTML(r io.Reader, base *url.URL) (title, text string) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", ""
	}
	title = pageTitle(doc)
	root := mainContent(doc)
	if root == nil {
		root = doc
	}
	w := &textWriter{base: base}
	w.walk(root)
	return title, normalizeText(w.b.String())
}

func pageTitle(doc *html.Node) string {
	var title, ogTitle string
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Title:
				if title == "" {
					title = collapseSpace(textOf(n))
				}
			case atom.Meta:
				if attr(n, "property") == "og:title" && ogTitle == "" {
					ogTitle = collapseSpace(attr(n, "content"))
				}
			case atom.Body, atom.Svg:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if title == "" {
		title = ogTitle
	}
	return clipRunes(title, 200)
}

// mainContent picks the element holding the page's main content: <main>
// or role=main, else the longest <article>; nil means "read the body".
func mainContent(doc *html.Node) *html.Node {
	var main, article *html.Node
	articleLen := 0
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipped(n) {
				return
			}
			if main == nil && (n.DataAtom == atom.Main || attr(n, "role") == "main") {
				main = n
			}
			if n.DataAtom == atom.Article {
				if l := len(strings.TrimSpace(textOf(n))); l > articleLen {
					article, articleLen = n, l
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	switch {
	case main != nil && len(strings.TrimSpace(textOf(main))) >= 200:
		return main
	case article != nil && articleLen >= 200:
		return article
	}
	return nil
}

// skipped elements never carry readable page content.
func skipped(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Math, atom.Canvas,
		atom.Iframe, atom.Object, atom.Embed, atom.Head, atom.Nav, atom.Footer, atom.Aside,
		atom.Form, atom.Button, atom.Select, atom.Option, atom.Input, atom.Textarea, atom.Dialog, atom.Menu:
		return true
	}
	if _, hidden := attrOK(n, "hidden"); hidden {
		return true
	}
	if attr(n, "aria-hidden") == "true" {
		return true
	}
	style := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

type textWriter struct {
	b    strings.Builder
	base *url.URL
	pre  int // inside <pre>
}

// block starts a new paragraph unless one was just started.
func (w *textWriter) block() {
	s := w.b.String()
	if s == "" || strings.HasSuffix(s, "\n\n") {
		return
	}
	if strings.HasSuffix(s, "\n") {
		w.b.WriteString("\n")
		return
	}
	w.b.WriteString("\n\n")
}

func (w *textWriter) line() {
	s := w.b.String()
	if s != "" && !strings.HasSuffix(s, "\n") {
		w.b.WriteString("\n")
	}
}

func (w *textWriter) write(text string) {
	if w.pre > 0 {
		w.b.WriteString(text)
		return
	}
	text = spaceRun.ReplaceAllString(text, " ")
	s := w.b.String()
	if s == "" || strings.HasSuffix(s, "\n") || strings.HasSuffix(s, " ") {
		text = strings.TrimLeft(text, " ")
	}
	w.b.WriteString(text)
}

func (w *textWriter) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.write(n.Data)
		return
	case html.ElementNode:
		if skipped(n) {
			return
		}
	case html.DocumentNode:
	default:
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		w.block()
		w.b.WriteString(strings.Repeat("#", int(n.Data[1]-'0')) + " ")
		w.children(n)
		w.block()
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Main, atom.Header, atom.Blockquote,
		atom.Figure, atom.Figcaption, atom.Details, atom.Summary, atom.Dl, atom.Address, atom.Ul, atom.Ol, atom.Table:
		w.block()
		w.children(n)
		w.block()
	case atom.Li:
		w.line()
		w.b.WriteString("- ")
		w.children(n)
		w.line()
	case atom.Dt, atom.Dd, atom.Tr, atom.Caption:
		w.line()
		w.children(n)
		w.line()
	case atom.Td, atom.Th:
		if s := w.b.String(); s != "" && !strings.HasSuffix(s, "\n") {
			w.b.WriteString(" | ")
		}
		w.children(n)
	case atom.Br:
		w.b.WriteString("\n")
	case atom.Hr:
		w.block()
	case atom.Pre:
		w.block()
		w.b.WriteString("```\n")
		w.pre++
		w.children(n)
		w.pre--
		w.line()
		w.b.WriteString("```")
		w.block()
	case atom.Code:
		if w.pre > 0 {
			w.children(n)
			return
		}
		w.write("`")
		w.children(n)
		w.b.WriteString("`")
	case atom.A:
		label := collapseSpace(textOf(n))
		href := w.resolve(attr(n, "href"))
		if label == "" || href == "" || href == label {
			w.children(n)
			return
		}
		w.write("[" + label + "](" + href + ")")
	case atom.Img:
		// Images aren't read; their alt text adds noise more often than not.
	default:
		w.children(n)
	}
}

func (w *textWriter) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}

// resolve returns href as an absolute http(s) URL, or "" for in-page
// anchors, javascript:, mailto: and the like.
func (w *textWriter) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if w.base != nil {
		u = w.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteString(" ")
			return
		}
		if n.Type == html.ElementNode && n.DataAtom != atom.Title && skipped(n) {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func attr(n *html.Node, key string) string {
	v, _ := attrOK(n, key)
	return v
}

func attrOK(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

var (
	spaceRun      = regexp.MustCompile(`[ \t\r\n\f\v\x{00a0}]+`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	blankRuns     = regexp.MustCompile(`\n{3,}`)
)

func collapseSpace(s string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

// normalizeText trims trailing spaces, unifies line endings and keeps at
// most one blank line in a row.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = trailingSpace.ReplaceAllString(s, "\n")
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
