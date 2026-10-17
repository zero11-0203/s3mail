package mailparse

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Bodies are the RFC 8621 §4.1.4 body lists.
type Bodies struct {
	Text        []*Part
	HTML        []*Part
	Attachments []*Part
}

func isInlineMediaType(t string) bool {
	return strings.HasPrefix(t, "image/") || strings.HasPrefix(t, "audio/") || strings.HasPrefix(t, "video/")
}

// BodyLists runs the parseStructure algorithm of RFC 8621 §4.1.4.
func (m *Message) BodyLists() Bodies {
	text, htmlb, att := []*Part{}, []*Part{}, []*Part{}
	parseStructure([]*Part{m.Body}, "mixed", false, &htmlb, &text, &att)
	return Bodies{Text: text, HTML: htmlb, Attachments: att}
}

// parseStructure mirrors the RFC pseudo-code. A nil list pointer stands for
// the algorithm's "null" (that list is no longer being filled).
func parseStructure(parts []*Part, multipartType string, inAlternative bool, htmlBody, textBody, attachments *[]*Part) {
	textLength, htmlLength := -1, -1
	if textBody != nil {
		textLength = len(*textBody)
	}
	if htmlBody != nil {
		htmlLength = len(*htmlBody)
	}
	for i, part := range parts {
		isMultipart := part.IsMultipart()
		isInline := part.Disposition != "attachment" &&
			(part.Type == "text/plain" || part.Type == "text/html" || isInlineMediaType(part.Type)) &&
			(i == 0 || (multipartType != "related" && (isInlineMediaType(part.Type) || part.Name == "")))

		switch {
		case isMultipart:
			sub := strings.TrimPrefix(part.Type, "multipart/")
			parseStructure(part.SubParts, sub, inAlternative || sub == "alternative", htmlBody, textBody, attachments)
		case isInline:
			if multipartType == "alternative" {
				switch part.Type {
				case "text/plain":
					*textBody = append(*textBody, part)
				case "text/html":
					*htmlBody = append(*htmlBody, part)
				default:
					*attachments = append(*attachments, part)
				}
				continue
			} else if inAlternative {
				if part.Type == "text/plain" {
					htmlBody = nil
				}
				if part.Type == "text/html" {
					textBody = nil
				}
			}
			if textBody != nil {
				*textBody = append(*textBody, part)
			}
			if htmlBody != nil {
				*htmlBody = append(*htmlBody, part)
			}
			if (textBody == nil || htmlBody == nil) && isInlineMediaType(part.Type) {
				*attachments = append(*attachments, part)
			}
		default:
			*attachments = append(*attachments, part)
		}
	}
	if multipartType == "alternative" && textBody != nil && htmlBody != nil {
		// Found HTML part only.
		if textLength == len(*textBody) && htmlLength != len(*htmlBody) {
			*textBody = append(*textBody, (*htmlBody)[htmlLength:]...)
		}
		// Found plaintext part only.
		if htmlLength == len(*htmlBody) && textLength != len(*textBody) {
			*htmlBody = append(*htmlBody, (*textBody)[textLength:]...)
		}
	}
}

// BodyValue is a JMAP EmailBodyValue.
type BodyValue struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// Value decodes a text part to UTF-8, truncated to maxBytes (0 = no limit)
// on a character boundary.
func (p *Part) Value(maxBytes int) BodyValue {
	s, problem := DecodeCharset(p.Charset, p.content)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	v := BodyValue{Value: s, IsEncodingProblem: problem}
	if maxBytes > 0 && len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		v.Value, v.IsTruncated = s[:cut], true
	}
	return v
}

var (
	tagPattern   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)
	spacePattern = regexp.MustCompile(`\s+`)
)

// VisibleText returns a text part's decoded text, truncated to maxBytes
// before tag removal; HTML loses its tags, scripts and styles.
func (p *Part) VisibleText(maxBytes int) string {
	s := p.Value(maxBytes).Value
	if p.Type == "text/html" {
		s = html.UnescapeString(tagPattern.ReplaceAllString(s, " "))
	}
	return s
}

// Preview returns up to 256 characters of plain text from the first
// textBody part that has any, for list display (RFC 8621 §4.1.4).
func (m *Message) Preview(b Bodies) string {
	for _, p := range b.Text {
		if !strings.HasPrefix(p.Type, "text/") {
			continue
		}
		s := p.VisibleText(64 << 10)
		s = strings.TrimSpace(spacePattern.ReplaceAllString(s, " "))
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > 256 {
			r := []rune(s)
			s = string(r[:256])
		}
		return s
	}
	return ""
}
