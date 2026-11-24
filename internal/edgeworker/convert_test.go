package edgeworker

import "testing"

func TestMetadataUnescapesTwiceEscapedTags(t *testing.T) {
	m := metadata(`<html><head><title>Cloud &amp;amp; Infra</title><meta property="og:title" content="Cloud &amp;amp; Infra"></head></html>`)
	if m["title"] != "Cloud & Infra" || m["ogTitle"] != "Cloud & Infra" {
		t.Errorf("metadata = %v", m)
	}
}

func TestMarkdownOfClassMissing(t *testing.T) {
	if md := markdownOfClass(`<p>x</p>`, "show-more-less-html__markup"); md != "" {
		t.Errorf("markdown = %q", md)
	}
}
