// Package mailparse reads stored raw MIME into the JMAP Email projection
// (RFC 8621 §4): header fields and their parsed forms, the body structure
// and the textBody/htmlBody/attachments lists. It never rewrites the raw
// message; display sanitization belongs to clients.
package mailparse

import (
	"bytes"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// lookup returns the decoder for a MIME charset name, covering the legacy
// charsets of the golden corpus (GBK, Big5, Shift_JIS, ISO-2022-JP, EUC-KR,
// windows-1252) through the WHATWG index.
func lookup(charset string) (encoding.Encoding, bool) {
	cs := strings.ToLower(strings.TrimSpace(charset))
	switch cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return nil, true
	}
	e, err := htmlindex.Get(cs)
	if err != nil {
		return nil, false
	}
	return e, true
}

// DecodeCharset converts b from charset to UTF-8. problem reports an
// unknown charset or bytes that did not decode cleanly; invalid sequences
// become U+FFFD.
func DecodeCharset(charset string, b []byte) (s string, problem bool) {
	e, ok := lookup(charset)
	if !ok {
		problem = true
	}
	if e != nil {
		out, _, err := transform.Bytes(e.NewDecoder(), b)
		if err != nil {
			problem = true
		} else {
			b = out
		}
	}
	if !utf8.Valid(b) {
		return string(bytes.ToValidUTF8(b, []byte("�"))), true
	}
	return string(b), problem
}

// WordDecoder decodes RFC 2047 encoded words in any charset lookup knows.
var WordDecoder = &mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		e, ok := lookup(charset)
		if !ok {
			return nil, errUnknownCharset
		}
		if e == nil {
			return input, nil
		}
		return transform.NewReader(input, e.NewDecoder()), nil
	},
}

type charsetError string

func (e charsetError) Error() string { return string(e) }

const errUnknownCharset = charsetError("unknown charset")
