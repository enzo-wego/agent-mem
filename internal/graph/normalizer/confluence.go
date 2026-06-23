package normalizer

import (
	"bytes"
	"context"
	"encoding/xml"
	"regexp"
	"strings"
)

// ConfluenceNormalizer converts Confluence storage format (XHTML-like XML) to plain text.
type ConfluenceNormalizer struct{}

// NewConfluenceNormalizer returns a ConfluenceNormalizer.
func NewConfluenceNormalizer() *ConfluenceNormalizer { return &ConfluenceNormalizer{} }

func (n *ConfluenceNormalizer) Source() string { return "confluence" }

// Normalize walks Confluence storage-format XML and emits plain text.
// Falls back to tag-strip + entity-unescape on parse errors.
func (n *ConfluenceNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	text, err := parseConfluenceXML(raw)
	if err != nil {
		// Fallback: strip tags and unescape entities.
		text = confluenceFallback(raw)
	}

	return Result{Text: text}, nil
}

func parseConfluenceXML(raw []byte) (string, error) {
	// Wrap in a root element so the tokeniser handles fragments.
	wrapped := append([]byte("<root>"), raw...)
	wrapped = append(wrapped, []byte("</root>")...)

	dec := xml.NewDecoder(bytes.NewReader(wrapped))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var b strings.Builder
	skipDepth := 0           // depth inside <style> or <script>
	inCode := false          // inside ac:structured-macro name="code"
	var listStack []bool     // true = ordered
	pendingListItem := false

	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			ns := t.Name.Space

			// Skip style/script content.
			if name == "style" || name == "script" {
				skipDepth++
				continue
			}
			if skipDepth > 0 {
				continue
			}

			switch name {
			case "br":
				b.WriteByte('\n')
			case "li":
				pendingListItem = true
				if len(listStack) > 0 && listStack[len(listStack)-1] {
					// ordered — prefix will be added on CharData; just mark
				}
				b.WriteString("- ")
			case "ul":
				listStack = append(listStack, false)
			case "ol":
				listStack = append(listStack, true)
			case "structured-macro":
				// ac:structured-macro ac:name="code"
				if ns == "ac" || strings.HasPrefix(t.Name.Space, "ac") {
					for _, attr := range t.Attr {
						if strings.ToLower(attr.Name.Local) == "name" && attr.Value == "code" {
							inCode = true
							b.WriteString("```")
						}
					}
				}
			}
			_ = pendingListItem

		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)

			if name == "style" || name == "script" {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if skipDepth > 0 {
				continue
			}

			switch name {
			case "p":
				b.WriteString("\n\n")
			case "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n\n")
			case "li":
				b.WriteByte('\n')
			case "ul", "ol":
				if len(listStack) > 0 {
					listStack = listStack[:len(listStack)-1]
				}
			case "structured-macro":
				if inCode {
					b.WriteString("```")
					inCode = false
				}
			}

		case xml.CharData:
			if skipDepth > 0 {
				continue
			}
			text := collapseWhitespace(string(t))
			if text != "" {
				b.WriteString(text)
			}
		}
	}

	result := strings.TrimSpace(b.String())
	// Collapse runs of 3+ newlines.
	result = regexp.MustCompile(`\n{3,}`).ReplaceAllString(result, "\n\n")
	return result, nil
}

// collapseWhitespace normalises runs of whitespace to a single space,
// but preserves newlines as single spaces too (inline context).
func collapseWhitespace(s string) string {
	return regexp.MustCompile(`[ \t\r\n]+`).ReplaceAllString(s, " ")
}

var stripTagsRe = regexp.MustCompile(`<[^>]+>`)

// confluenceFallback strips XML tags and unescapes common HTML entities.
func confluenceFallback(raw []byte) string {
	s := stripTagsRe.ReplaceAllString(string(raw), " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
