package channels

import (
	"html"
	"mime"
)

// htmlUnescapeString wraps the stdlib html.UnescapeString for use
// through the email channel's htmlToText pipeline. Kept as a named
// function so callers don't import html directly in every file.
func htmlUnescapeString(s string) string {
	return html.UnescapeString(s)
}

// newMimeWordDecoder returns a *mime.WordDecoder configured to decode
// RFC 2047 Q- and B-encoded header words into UTF-8. Used by
// decodeRFC2047Header. The zero value handles both encodings.
func newMimeWordDecoder() *mime.WordDecoder {
	return &mime.WordDecoder{}
}
