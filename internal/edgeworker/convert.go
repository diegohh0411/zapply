package edgeworker

import (
	"strings"
	"unicode/utf8"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"golang.org/x/net/html"
)

// Result sizes stay under the API's limits (1 MiB markdown, 4 MiB HTML).
const (
	maxMarkdown = 900 << 10
	maxHTML     = 3500 << 10
)

// toMarkdown converts a page to markdown, without its scripts, styles and
// other non-content elements.
func toMarkdown(page string) string {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return ""
	}
	prune(doc)
	var sb strings.Builder
	if err := html.Render(&sb, doc); err != nil {
		return ""
	}
	md, err := htmltomarkdown.ConvertString(sb.String())
	if err != nil {
		return ""
	}
	return truncate(strings.TrimSpace(md), maxMarkdown)
}

func prune(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode {
			switch c.Data {
			case "script", "style", "noscript", "svg", "iframe", "template", "link", "head":
				n.RemoveChild(c)
				c = next
				continue
			}
		}
		prune(c)
		c = next
	}
}

// metadata reads the page's title and Open Graph tags under the names the
// API's scrapers use (ogTitle, ogDescription, ...).
func metadata(page string) map[string]any {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	m := map[string]any{}
	names := map[string]string{"og:title": "ogTitle", "og:description": "ogDescription", "og:site_name": "ogSiteName", "og:url": "ogUrl", "og:image": "ogImage"}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if _, ok := m["title"]; !ok && n.FirstChild != nil {
					m["title"] = truncate(strings.TrimSpace(n.FirstChild.Data), 500)
				}
			case "meta":
				key := attr(n, "property")
				if key == "" {
					key = attr(n, "name")
				}
				if to, ok := names[strings.ToLower(key)]; ok {
					if v := strings.TrimSpace(attr(n, "content")); v != "" {
						if _, set := m[to]; !set {
							m[to] = truncate(v, 2000)
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return m
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textOfClass returns the text of the first element carrying class.
func textOfClass(page, class string) string {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return ""
	}
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode {
			for _, c := range strings.Fields(attr(n, "class")) {
				if c == class {
					found = n
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if found == nil {
		return ""
	}
	var sb strings.Builder
	var text func(*html.Node)
	text = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			text(c)
		}
	}
	text(found)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
