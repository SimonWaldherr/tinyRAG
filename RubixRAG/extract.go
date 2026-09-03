package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/encoding/charmap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Multi-format text extraction
//
// Plain-text-ish formats are parsed natively (no external process). Office
// documents, PDFs, images and everything else markitdown understands are
// converted by shelling out to the `markitdown` CLI
// (https://github.com/microsoft/markitdown, `pip install markitdown`),
// following the same "shell out to an existing tool" pattern tinyRAG uses
// for its SQL modules. This keeps R3 itself free of cgo/PDF/OOXML parsing
// dependencies while still covering PPTX/DOCX/XLSX/PDF/images/audio/etc.
// ─────────────────────────────────────────────────────────────────────────────

// nativeTextExtensions lists extensions read and used as-is, without going
// through markitdown — plain text/code/config formats where no conversion
// is needed.
func nativeTextExtensions() map[string]bool {
	return map[string]bool{
		".txt": true, ".md": true, ".csv": true, ".tsv": true,
		".json": true, ".jsonl": true, ".ndjson": true,
		".xml": true, ".log": true, ".yaml": true, ".yml": true,
		".toml": true, ".ini": true, ".cfg": true, ".conf": true,
		".sql": true, ".go": true, ".py": true, ".js": true, ".ts": true,
		".css": true,
	}
}

// nativeHTMLExtensions lists extensions run through htmlToText directly
// rather than markitdown, since Go's own tag-stripping is enough for plain
// HTML files.
func nativeHTMLExtensions() map[string]bool {
	return map[string]bool{".html": true, ".htm": true}
}

// markItDownExtensions lists formats handed to the markitdown CLI. Not
// exhaustive of what markitdown supports — extend as needed.
func markItDownExtensions() map[string]bool {
	return map[string]bool{
		".docx": true, ".doc": true, ".pptx": true, ".ppt": true,
		".docm": true, ".xlsm": true, ".pptm": true,
		".xlsx": true, ".xls": true, ".pdf": true, ".rtf": true,
		".odt": true, ".ods": true, ".odp": true, ".epub": true,
		".msg": true,
		// MarkItDown's optional audio converter handles these when its
		// audio-transcription extra is installed. Video is handled through
		// ffmpeg below so the converter receives a supported audio file.
		".wav": true, ".mp3": true, ".m4a": true, ".flac": true,
		".ogg": true, ".aac": true,
	}
}

func archiveExtensions() map[string]bool {
	return map[string]bool{
		".zip": true, ".7z": true, ".tar": true, ".tar.gz": true, ".tgz": true,
	}
}

func subtitleExtensions() map[string]bool {
	return map[string]bool{".srt": true, ".vtt": true}
}

// fileExtension preserves compound archive suffixes that filepath.Ext would
// otherwise reduce to just ".gz". All import paths use this helper so folder
// walks, uploads, chat attachments and server-side extraction agree.
func fileExtension(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar"} {
		if strings.HasSuffix(lower, suffix) {
			return suffix
		}
	}
	return strings.ToLower(filepath.Ext(lower))
}

// imageExtensions lists image formats routed through tesseract OCR
// (extractImageTextOCR) on the upload/folder path — the same treatment a
// mail-shaped attachment already gets via extractAttachmentText's
// content-type sniffing (ingest.go). Before this branch existed, uploading
// the very same scanned invoice that imports fine as an email attachment
// was rejected as "unsupported file type".
func imageExtensions() map[string]bool {
	return map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
		".bmp": true, ".tif": true, ".tiff": true, ".webp": true,
	}
}

// extractText reads path and returns plain text/markdown suitable for
// chunking, dispatching on file extension. It returns an error for
// unsupported extensions or files exceeding maxMB.
func extractText(path string, s appSettings) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory")
	}
	maxMB := s.Import.MaxFileMB
	if maxMB <= 0 {
		maxMB = 25
	}
	if info.Size() > maxMB*1024*1024 {
		return "", fmt.Errorf("file too large (%d bytes, limit %d MB)", info.Size(), maxMB)
	}

	ext := fileExtension(path)
	text, err := extractTextByExt(path, ext, s)
	if err != nil {
		return "", err
	}
	// repairMojibake is a defensive backstop against charset corruption
	// this package's own decoding didn't already prevent — see
	// encoding.go. Cheap and a no-op on already-clean text.
	return repairMojibake(text), nil
}

// extractTextByExt dispatches path to the right extraction strategy for
// ext: native .eml parsing, plain-text passthrough, native HTML stripping,
// or shelling out to markitdown — in that preference order, so formats R3
// can handle natively never pay the external-process cost.
func extractTextByExt(path, ext string, s appSettings) (string, error) {
	switch {
	case ext == ".eml":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractEML(b)
	case ext == ".mbox":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractMBOX(b)
	case ext == ".mht" || ext == ".mhtml":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractMHTML(b)
	case ext == ".vcf":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractVCards(b)
	case ext == ".ics":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractCalendar(b)
	case subtitleExtensions()[ext]:
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractSubtitles(b, ext), nil
	case archiveExtensions()[ext]:
		if !s.AllowShellExec && ext == ".7z" {
			return "", fmt.Errorf("extracting %s requires allow_shell_exec=true (runs the configured 7z binary)", ext)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractArchive(path, b, ext, s)
	case isVideoExtension(ext):
		if !s.AllowShellExec {
			return "", fmt.Errorf("extracting %s requires allow_shell_exec=true (runs ffmpeg and the audio transcription converter)", ext)
		}
		return runVideoToText(s.Import.FFmpegBin, s.Import.MarkItDownBin, path, s.Import.MaxFileMB)
	case nativeTextExtensions()[ext]:
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case nativeHTMLExtensions()[ext]:
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return htmlToText(string(b)), nil
	case markItDownExtensions()[ext]:
		if !s.AllowShellExec {
			return "", fmt.Errorf("extracting %s requires allow_shell_exec=true (runs the external markitdown CLI)", ext)
		}
		return runMarkItDown(s.Import.MarkItDownBin, path, s.Import.MarkItDownDocIntelEndpoint)
	case imageExtensions()[ext]:
		if !s.AllowShellExec {
			return "", fmt.Errorf("extracting %s requires allow_shell_exec=true (runs the external tesseract CLI for OCR)", ext)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return extractImageTextOCR(b, filepath.Base(path), s.Import, s.AllowShellExec)
	default:
		return "", fmt.Errorf("unsupported file type: %s", ext)
	}
}

// runMarkItDown shells out to Microsoft's markitdown converter
// (https://github.com/microsoft/markitdown) and returns its markdown output.
//
// Known limitation: PDFs with rotated/vertical sidebar text (e.g. a
// datasheet's "subject to change without notice" disclaimer printed
// sideways along the page margin) can come back as long runs of
// single-character lines, in reverse reading order — an artifact of how
// pdfminer (which markitdown uses for PDF parsing) walks glyph runs at
// non-horizontal angles. This is not mojibake/encoding corruption (each
// character decodes correctly, just in the wrong order/segmentation) and
// repairMojibake in encoding.go doesn't address it. No repair is applied
// here deliberately: a heuristic to collapse such runs would risk mangling
// legitimate short-line content (numbered lists, code, table columns)
// elsewhere in the corpus.
func runMarkItDown(bin, path, docIntelEndpoint string) (string, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "markitdown"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("markitdown binary %q not found in PATH (install with 'pip install markitdown'): %w", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	args := []string{path}
	if endpoint := strings.TrimSpace(docIntelEndpoint); endpoint != "" {
		args = append(args, "-d", "-e", endpoint)
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("markitdown timed out on %s", path)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("markitdown failed on %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("markitdown failed on %s: %w", path, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("markitdown returned no text for %s", path)
	}
	return text, nil
}

// isExtractableDocument reports whether ext is a format extractTextByExt
// already knows how to handle — used by chatimages.go's decodeAskImages to
// decide whether a non-image Chat/Agent attachment (PDF, Office file,
// plain text, ...) can be routed to extractDocumentText below instead of
// being rejected outright as "not an image".
func isExtractableDocument(ext string) bool {
	return ext == ".eml" || ext == ".mbox" || ext == ".mht" || ext == ".mhtml" || ext == ".vcf" || ext == ".ics" ||
		nativeTextExtensions()[ext] || nativeHTMLExtensions()[ext] || markItDownExtensions()[ext] || subtitleExtensions()[ext] || archiveExtensions()[ext] || isVideoExtension(ext)
}

// extractDocumentText text-extracts an in-memory Chat/Agent document
// attachment (a Chat/Agent upload, never written anywhere by the caller
// beyond this temp file) — mirrors extractImageTextOCR's exact "write to a
// named temp file, delegate, clean up regardless" pattern just below, but
// hands off to extractTextByExt's existing extension dispatch instead of
// tesseract directly, so a document attachment gets exactly the same
// extraction Import already trusts for the same file types (native text/
// HTML/.eml need no external process; PDF/Office go through markitdown,
// gated behind allowShellExec exactly as extractTextByExt already
// enforces).
func extractDocumentText(data []byte, filename string, importCfg importConfig, allowShellExec bool) (string, error) {
	ext := fileExtension(filename)
	tmp, err := os.CreateTemp("", "r3-attach-*"+ext)
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	text, err := extractTextByExt(tmp.Name(), ext, appSettings{Import: importCfg, AllowShellExec: allowShellExec})
	if err != nil {
		return "", err
	}
	return repairMojibake(text), nil
}

// extractImageTextOCR OCRs an in-memory image (a Chat/Agent upload,
// never written anywhere by the caller) via runTesseractOCR — writes it
// to a temp file first since tesseract's own stdin support is less
// portable across versions/platforms than reading a real file path, then
// removes it regardless of outcome. Gated behind allowShellExec, same
// risk class and same flag (settings.AllowShellExec) as markitdown
// above: an external process invoked with a file path.
func extractImageTextOCR(data []byte, filename string, cfg importConfig, allowShellExec bool) (string, error) {
	if !allowShellExec {
		return "", fmt.Errorf("OCR ist deaktiviert (Einstellungen → Import → „markitdown-/tesseract-Aufrufe erlauben“)")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".png"
	}
	tmp, err := os.CreateTemp("", "r3-ocr-*"+ext)
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	return runTesseractOCR(cfg.TesseractBin, cfg.TesseractLang, tmp.Name())
}

// runTesseractOCR shells out to tesseract (https://github.com/tesseract-ocr/tesseract,
// install via apt/brew/choco), mirroring runMarkItDown's exact pattern —
// LookPath, a bounded timeout, and a stderr-aware error message — since
// it's the same kind of external-CLI-as-text-extractor call, just for
// images/scans instead of office documents.
func runTesseractOCR(bin, lang, path string) (string, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "tesseract"
	}
	if strings.TrimSpace(lang) == "" {
		lang = "deu+eng"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("tesseract binary %q not found in PATH (install tesseract-ocr): %w", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, path, "stdout", "-l", lang)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("tesseract timed out on %s", path)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("tesseract failed on %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("tesseract failed on %s: %w", path, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("tesseract found no text in %s", path)
	}
	return text, nil
}

var (
	htmlTagRe    = regexp.MustCompile(`<[^>]*>`)
	multiSpaceRe = regexp.MustCompile(`\s{3,}`)
)

// collapseRepeatedRuns scans s and, for any MAXIMAL run of 4+ repeats of the
// exact same rune, truncates that run down to exactly 3 occurrences — but
// ONLY when that rune is neither a letter nor a digit
// (unicode.IsLetter/IsDigit both false). This scoping is deliberate and must
// not be relaxed: a part number, serial number, or price can legitimately
// contain a long run of the same digit or letter (e.g. "AAAA-1000",
// "0000042"), and collapsing those would silently corrupt real data. What
// this DOES target is punctuation/symbol/whitespace runs that exist purely
// for visual formatting in the source document and carry zero information
// beyond "there's a separator here" — a PDF table-of-contents dot-leader
// ("Kapitel 3 .......................... 42") or a markdown table's
// alignment row ("| ------ | -------- |"), both of which cost real
// embedding/context tokens at full length for no semantic benefit.
//
// Shared by two call sites (see their own comments for why each needs it
// independently): ingestDocument (ingest.go) applies it once at extraction
// time, benefiting embedding cost/chunk quality for future imports;
// rank.go's assembleContext/expandEmailFamilies apply it again at
// context-assembly time, benefiting content already in the knowledge base
// (already-embedded/chunked text isn't re-collapsed retroactively) without
// needing a re-import.
//
// Ranges over s as runes (not bytes), so multi-byte UTF-8 runes (e.g. a run
// of em-dashes "——————") are never split mid-rune.
func collapseRepeatedRuns(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	var prev rune
	count := 0
	first := true
	for _, r := range s {
		if !first && r == prev {
			count++
		} else {
			count = 1
			prev = r
			first = false
		}
		if count <= 3 || unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// htmlToText heuristically strips tags/scripts/styles from HTML, matching
// tinyRAG's URL-fetching stripper.
func htmlToText(rawHTML string) string {
	text := rawHTML
	for _, tag := range []string{"script", "style", "nav", "footer", "header"} {
		re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>.*?</` + tag + `>`)
		text = re.ReplaceAllString(text, " ")
	}
	text = htmlTagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = multiSpaceRe.ReplaceAllString(text, "\n")
	return strings.TrimSpace(text)
}

// rtfSkipDestinations lists RTF control words whose group content is never
// visible document text (font/color tables, embedded objects, generator
// info, ...) — encountering one of these as the first control word in a
// group marks that whole group (and any nested sub-groups) as skipped.
var rtfSkipDestinations = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true,
	"generator": true, "pict": true, "object": true, "themedata": true,
	"colorschememapping": true, "latentstyles": true, "rsid": true,
	"xmlnstbl": true, "listtable": true, "listoverridetable": true,
	"datastore": true, "nonshppict": true, "header": true, "footer": true,
}

var (
	rtfBlankRunsRe = regexp.MustCompile(`\n{3,}`)
	rtfSpaceRunsRe = regexp.MustCompile(`[ \t]{2,}`)
)

// rtfToText heuristically strips an RTF document down to its visible text,
// used as a fallback for PST messages whose body was only ever composed as
// rich text (PidTagRtfCompressed) — go-pst decompresses that to raw RTF
// markup (see message.GetBodyRTF), but doesn't render it to plain text.
// Not a full RTF parser: it tracks group nesting/skip-destinations and the
// handful of control words that matter for extracted text (\par, \line,
// \tab, \'hh hex-escaped bytes, \uN unicode escapes), same "good enough for
// RAG" spirit as htmlToText above. \uc (unicode fallback skip count) is
// assumed to always be 1, which covers virtually everything Outlook/Word
// actually generate.
func rtfToText(rtf string) string {
	data := []byte(rtf)
	n := len(data)
	var out strings.Builder

	// Plain literal bytes and \'hh escapes both end up here — RTF requires
	// {}\  to be escaped in running text, so unescaped literal bytes are
	// always plain ASCII and decode identically under Windows-1252,
	// letting both paths share one cp1252->UTF-8 flush.
	var pending []byte
	flush := func() {
		if len(pending) == 0 {
			return
		}
		if decoded, err := charmap.Windows1252.NewDecoder().Bytes(pending); err == nil {
			out.Write(decoded)
		} else {
			out.Write(pending)
		}
		pending = nil
	}

	var skipStack []bool
	skipping := func() bool { return len(skipStack) > 0 && skipStack[len(skipStack)-1] }
	isAlpha := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }

	// skipUnicodeFallback consumes the one ANSI fallback "unit" (per \uc1,
	// see doc comment) that RTF requires immediately after every \uN.
	skipUnicodeFallback := func(i int) int {
		if i >= n {
			return i
		}
		if data[i] == '\\' && i+1 < n && data[i+1] == '\'' && i+4 <= n {
			return i + 4
		}
		if data[i] == '\\' || data[i] == '{' || data[i] == '}' {
			return i
		}
		return i + 1
	}

	i := 0
	for i < n {
		switch data[i] {
		case '{':
			flush()
			skipStack = append(skipStack, skipping())
			i++
		case '}':
			flush()
			if len(skipStack) > 0 {
				skipStack = skipStack[:len(skipStack)-1]
			}
			i++
		case '\\':
			flush()
			i++
			if i >= n {
				break
			}
			switch data[i] {
			case '\\', '{', '}':
				if !skipping() {
					pending = append(pending, data[i])
				}
				i++
			case '\r', '\n':
				i++
			case '\'':
				i++
				if i+2 <= n {
					if b, err := strconv.ParseUint(string(data[i:i+2]), 16, 8); err == nil && !skipping() {
						pending = append(pending, byte(b))
					}
					i += 2
				}
			case '*':
				if len(skipStack) > 0 {
					skipStack[len(skipStack)-1] = true
				}
				i++
			default:
				start := i
				for i < n && isAlpha(data[i]) {
					i++
				}
				word := string(data[start:i])
				numStart := i
				if i < n && data[i] == '-' {
					i++
				}
				for i < n && isDigit(data[i]) {
					i++
				}
				num := string(data[numStart:i])
				if i < n && data[i] == ' ' {
					i++
				}
				if word != "" && rtfSkipDestinations[word] && len(skipStack) > 0 {
					skipStack[len(skipStack)-1] = true
				}
				if skipping() {
					continue
				}
				switch word {
				case "par", "line":
					out.WriteByte('\n')
				case "tab":
					out.WriteByte('\t')
				case "u":
					if code, err := strconv.Atoi(num); err == nil {
						if code < 0 {
							code += 65536
						}
						out.WriteRune(rune(code))
						i = skipUnicodeFallback(i)
					}
				}
			}
		default:
			if !skipping() {
				pending = append(pending, data[i])
			}
			i++
		}
	}
	flush()

	text := rtfSpaceRunsRe.ReplaceAllString(out.String(), " ")
	text = rtfBlankRunsRe.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// emailFields is the normalized shape both .eml files and PST messages are
// converted into before being handed to the chunker. JSON tags matter here:
// draftReply.OriginalMail (draft.go) exposes this directly to the Mail
// tab's frontend, so it follows the same snake_case convention as every
// other API response instead of Go's default capitalized field names.
type emailFields struct {
	Subject string    `json:"subject"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Date    time.Time `json:"date"`
	Body    string    `json:"body"`
}

// String renders the email as a single document body with a small header
// block, so subject/sender/date are always part of what gets embedded and
// can be cited/searched on.
func (e emailFields) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\n", e.From)
	if e.To != "" {
		fmt.Fprintf(&b, "To: %s\n", e.To)
	}
	if !e.Date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\n", e.Date.Format(time.RFC1123Z))
	}
	fmt.Fprintf(&b, "Subject: %s\n\n%s", e.Subject, strings.TrimSpace(e.Body))
	return b.String()
}

// extractEML parses a raw RFC 5322 message (as stored in .eml files and
// exported by many mail clients) into plain text.
func extractEML(raw []byte) (string, error) {
	fields, err := emlToFields(raw)
	if err != nil {
		return "", err
	}
	return fields.String(), nil
}

// emlToFields parses a raw RFC 5322 message into structured emailFields,
// shared by extractEML (.eml file import) and imapmail.go (live IMAP
// fetch, which gets the same raw RFC822 bytes straight off the wire).
func emlToFields(raw []byte) (emailFields, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return emailFields{}, fmt.Errorf("parse eml: %w", err)
	}
	fields := emailFields{
		Subject: decodeMIMEHeader(msg.Header.Get("Subject")),
		From:    decodeMIMEHeader(msg.Header.Get("From")),
		To:      decodeMIMEHeader(msg.Header.Get("To")),
	}
	if d, err := msg.Header.Date(); err == nil {
		fields.Date = d
	}
	body, err := readMailBody(msg.Header.Get("Content-Type"), msg.Body)
	if err != nil {
		return emailFields{}, err
	}
	fields.Body = body
	return fields, nil
}

// mailAttachment is one attachment part pulled out of a raw RFC 5322
// message by extractMailAttachments, decoded from its declared
// Content-Transfer-Encoding — ready to hand straight to
// ingestEmailAttachment (ingest.go).
type mailAttachment struct {
	Filename string
	Data     []byte
}

// extractMailAttachments walks a raw RFC 5322 message's multipart
// structure (the same one level readMailBody handles) and returns every
// part that carries a filename — i.e. an attachment, not the text/plain or
// text/html body part readMailBody already extracts. A single-part message
// (no multipart Content-Type at all) can't carry attachments, so that case
// returns an empty slice rather than an error. Shared by imapmail.go (live
// IMAP fetch) and, indirectly, by extractEML's callers that also want
// attachments (.eml file import doesn't currently ingest attachments
// separately, matching upload's "one file, one source" model).
//
// maxBytes bounds how much of each part is read (<=0 falls back to a 50MB
// default, the previous hardcoded ceiling). An attachment over the limit is
// no longer silently truncated into corrupted, still-ingested data —
// instead it's skipped entirely and named in the returned warnings slice,
// since embedding a truncated PDF/base64 blob is worse than just not
// including that one attachment.
func extractMailAttachments(raw []byte, maxBytes int64) ([]mailAttachment, []string, error) {
	if maxBytes <= 0 {
		maxBytes = 50 * 1024 * 1024
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil, nil, fmt.Errorf("parse eml: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, nil
	}
	mr := multipart.NewReader(msg.Body, boundary)
	var atts []mailAttachment
	var warnings []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		filename := attachmentFilename(part)
		if filename == "" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxBytes+1))
		if err != nil {
			continue
		}
		if int64(len(data)) > maxBytes {
			warnings = append(warnings, fmt.Sprintf("%s: zu groß (>%d MB), übersprungen", decodeMIMEHeader(filename), maxBytes/(1024*1024)))
			continue
		}
		data = decodeTransferEncoding(data, part.Header.Get("Content-Transfer-Encoding"))
		atts = append(atts, mailAttachment{Filename: decodeMIMEHeader(filename), Data: data})
	}
	return atts, warnings, nil
}

// attachmentFilename returns a part's attachment filename, empty for parts
// that aren't attachments (the text/plain or text/html body readMailBody
// already handles). Part.FileName() covers the common
// "Content-Disposition: attachment; filename=..." case (including RFC 2231
// encoding); the Content-Type "name" parameter is a fallback some senders
// use instead.
func attachmentFilename(part *multipart.Part) string {
	if fn := part.FileName(); fn != "" {
		return fn
	}
	_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
	return params["name"]
}

// decodeTransferEncoding reverses a MIME part's declared
// Content-Transfer-Encoding — mime/multipart hands back the still-encoded
// bytes as-is, unlike the charset handling readMailBody does for text
// parts. base64 is by far the most common encoding for binary attachments;
// quoted-printable occasionally shows up too. An unrecognized/empty
// encoding (or a decode failure) returns data unchanged rather than
// failing the whole attachment.
func decodeTransferEncoding(data []byte, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		clean := make([]byte, 0, len(data))
		for _, b := range data {
			if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
				continue
			}
			clean = append(clean, b)
		}
		decoded, err := base64.StdEncoding.DecodeString(string(clean))
		if err != nil {
			return data
		}
		return decoded
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return data
		}
		return out
	default:
		return data
	}
}

// readMailBody extracts the best-effort plain text body from a message,
// walking one level of multipart/* to find a text/plain (falling back to
// text/html, stripped) part. Nested multiparts and attachments are ignored;
// this is a text extractor, not a MIME renderer. Every part's declared
// charset (the Content-Type "charset" parameter) is honored via
// decodeCharset — previously this parameter was parsed and then silently
// discarded, so a genuinely windows-1252/iso-8859-1 body would come out as
// mojibake (see encoding.go for the fuller explanation and the defensive
// backstop that also catches corruption from outside this function).
func readMailBody(contentType string, body io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		raw, err := io.ReadAll(io.LimitReader(body, 4*1024*1024))
		if err != nil {
			return "", err
		}
		text := decodeCharset(raw, params["charset"])
		if strings.Contains(strings.ToLower(mediaType), "html") {
			return htmlToText(text), nil
		}
		return text, nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		raw, err := io.ReadAll(io.LimitReader(body, 4*1024*1024))
		return decodeCharset(raw, params["charset"]), err
	}
	mr := multipart.NewReader(body, boundary)
	var plain, htmlPart string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		partType := part.Header.Get("Content-Type")
		pt, ptParams, _ := mime.ParseMediaType(partType)
		data, _ := io.ReadAll(io.LimitReader(part, 2*1024*1024))
		text := decodeCharset(data, ptParams["charset"])
		switch {
		case strings.HasPrefix(pt, "text/plain") && plain == "":
			plain = text
		case strings.HasPrefix(pt, "text/html") && htmlPart == "":
			htmlPart = text
		}
	}
	if plain != "" {
		return plain, nil
	}
	if htmlPart != "" {
		return htmlToText(htmlPart), nil
	}
	return "", nil
}
