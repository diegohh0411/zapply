package ats

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlText turns a board's HTML description into plain text, one block per
// line. Greenhouse sends the HTML entity-escaped, so it is unescaped first.
func htmlText(s string) string {
	if s == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(html.UnescapeString(s)))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			switch n.Data {
			case "script", "style":
				return
			case "br":
				b.WriteByte('\n')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "li", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6", "tr", "section":
				b.WriteByte('\n')
			}
		}
	}
	walk(doc)
	var lines []string
	for l := range strings.SplitSeq(b.String(), "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
