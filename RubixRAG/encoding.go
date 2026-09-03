package main

import (
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// ─────────────────────────────────────────────────────────────────────────────
// Charset handling
//
// Two complementary fixes for the mojibake ("MeineÂ Zeitbuchungen â
// Schnellerfassung" instead of "... – Schnellerfassung") seen in imported
// mail content:
//
//  1. decodeCharset actually honors a message's declared charset instead
//     of treating every byte string as already-UTF-8 (readMailBody in
//     extract.go used to discard the parsed charset parameter entirely).
//  2. repairMojibake is a defensive backstop for text that's *already*
//     corrupted by the time it reaches us — e.g. from a PST's internal
//     codepage handling, which R3 doesn't control — by detecting and
//     reversing the classic "UTF-8 bytes decoded one-byte-at-a-time as
//     Latin-1" pattern.
//
// Both are applied once, right after extraction (pst.go/extract.go), not
// sprinkled across every caller.
// ─────────────────────────────────────────────────────────────────────────────

// decodeCharset converts data to a UTF-8 string using the declared
// charset. An empty/utf-8/us-ascii charset is a no-op (data is assumed to
// already be UTF-8, which is overwhelmingly the common case and avoids any
// decoder overhead for it). Unknown charsets or decode failures fall back
// to treating the bytes as UTF-8 rather than erroring out — a best-effort
// text extractor should never fail an entire import over one odd header.
func decodeCharset(data []byte, charset string) string {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "utf8" || cs == "us-ascii" || cs == "ascii" {
		return string(data)
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return string(data)
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return string(data)
	}
	return string(out)
}

// mimeWordDecoder decodes RFC 2047 "encoded-word" header syntax
// (=?charset?Q?...?= / =?charset?B?...?=) — Go's net/mail deliberately
// does NOT do this for Header.Get(), so a Subject/From/To transmitted
// that way (still common from older mail systems) would otherwise come
// through as the literal "=?windows-1252?Q?...?=" text. The CharsetReader
// hook covers charsets beyond the handful Go's mime package understands
// natively, reusing the same x/text lookup as decodeCharset.
var mimeWordDecoder = &mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(strings.ToLower(strings.TrimSpace(charset)))
		if err != nil {
			return input, nil // unknown charset: best-effort passthrough rather than failing the whole header
		}
		return enc.NewDecoder().Reader(input), nil
	},
}

// decodeMIMEHeader decodes an RFC 2047 encoded-word header value; a plain
// (non-encoded) header is returned unchanged.
func decodeMIMEHeader(s string) string {
	decoded, err := mimeWordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return decoded
}

// cp1252ToByte maps every rune some Windows-1252 byte (0x80-0xFF) decodes
// to, back to that byte. Built once from the real charmap rather than
// assumed, because Windows-1252 is *not* a straight byte-value=rune-
// value (ISO-8859-1) mapping in the 0x80-0x9F range: e.g. byte 0x96
// decodes to "–" (U+2013), byte 0x80 to "€" (U+20AC), byte 0x95 to "•"
// (U+2022) — none of which fall in the 0x80-0xFF rune range a naive
// Latin-1 assumption would look for. repairMojibake needs the real
// mapping to catch mojibake involving those characters (dashes, curly
// quotes, bullets, €, ™, …) at all.
var cp1252ToByte = buildCP1252ReverseMap()

// buildCP1252ReverseMap constructs cp1252ToByte from the real Windows-1252
// decode table rather than a hardcoded literal, so it stays correct even
// for the non-Latin-1 runes in the 0x80-0x9F byte range (see cp1252ToByte's
// doc comment).
func buildCP1252ReverseMap() map[rune]byte {
	m := make(map[rune]byte, 128)
	dec := charmap.Windows1252.NewDecoder()
	for i := 0x80; i <= 0xFF; i++ {
		out, err := dec.Bytes([]byte{byte(i)})
		if err != nil || len(out) == 0 {
			continue
		}
		r, size := utf8.DecodeRune(out)
		if r != utf8.RuneError && size == len(out) {
			m[r] = byte(i)
		}
	}
	return m
}

// repairMojibake detects and reverses "UTF-8 encoded as Windows-1252"
// double decoding: genuine multi-byte UTF-8 (e.g. an en-dash, "–" = bytes
// 0xE2 0x80 0x93) that got decoded one byte at a time under Windows-1252
// shows up as a *run of 2 or more consecutive runes* that are each some
// cp1252 byte's decoded form. Re-encoding just that run back to raw bytes
// (via cp1252ToByte) and re-decoding it as UTF-8 recovers the original
// character.
//
// This deliberately operates on runs, not the whole string: a *single*
// isolated rune in that set is far more likely a legitimate character —
// ä/ö/ü/ß and an actual "–" or "…" all show up standalone constantly in
// German mail content. Treating the whole string as one blob (an earlier
// version of this function did) means one genuine "ü" anywhere else in
// the text breaks the round-trip and silently disables the repair for the
// *actual* mojibake elsewhere in the same string. Scoping to runs avoids
// that: an isolated "ü" is left completely alone, while a run of 2+ such
// runes — which only happens when multi-byte UTF-8 got split — gets
// reinterpreted, and only kept if that reinterpretation is itself valid
// UTF-8 (an extremely reliable signal at run lengths ≥2).
func repairMojibake(s string) string {
	if s == "" || !utf8.ValidString(s) {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	changed := false

	inCP1252 := func(r rune) bool {
		_, ok := cp1252ToByte[r]
		return ok
	}

	for i := 0; i < len(runes); {
		if !inCP1252(runes[i]) {
			b.WriteRune(runes[i])
			i++
			continue
		}
		j := i
		for j < len(runes) && inCP1252(runes[j]) {
			j++
		}
		if j-i < 2 {
			b.WriteRune(runes[i])
			i++
			continue
		}
		raw := make([]byte, j-i)
		for k := i; k < j; k++ {
			raw[k-i] = cp1252ToByte[runes[k]]
		}
		if utf8.Valid(raw) {
			b.Write(raw)
			changed = true
		} else {
			for k := i; k < j; k++ {
				b.WriteRune(runes[k])
			}
		}
		i = j
	}

	if !changed {
		return s
	}
	return b.String()
}
