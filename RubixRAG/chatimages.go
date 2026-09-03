package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Turning an uploaded Chat/Agent attachment into whatever the active chat
// backend can actually use. Two kinds, decided per-file in decodeAskImages:
//   - An actual image becomes either a real vision content part (when the
//     resolved profile's llmProfile.SupportsVision is true, see
//     settings.go) or OCR'd plain text folded into the question
//     (extractImageTextOCR, extract.go) when it isn't.
//   - A document (PDF, Office file, plain text, ...) is never sent as a
//     vision image_url part — even a vision-capable backend has no
//     sensible way to "see" a PDF as a picture — so it's always
//     text-extracted (extractDocumentText, extract.go, the exact same
//     markitdown-backed pipeline Import already trusts) and folded into
//     the question as plain text, independent of the image branch above.
//
// Deliberately ephemeral either way: nothing here ever touches the vector
// store/chunk pipeline (that's ingest.go's job) or any persisted chat
// history — an attachment affects only the one /api/ask request it rode
// in on.
// ─────────────────────────────────────────────────────────────────────────────

// askImageInput is one file attached to a Chat/Agent question
// (askRequest.Images) — an image or a document, see the package comment
// above. Raw bytes arrive base64-encoded over JSON, the same convention as
// every other binary-over-JSON field in R3. Named "image" for historical/
// wire-compatibility reasons (this predates document support) rather than
// the more accurate "attachment".
type askImageInput struct {
	Filename   string `json:"filename"`
	DataBase64 string `json:"data_base64"`
}

// uploadRouting is resolveUploadRouting's result — see below.
type uploadRouting struct {
	// Profile is the profile the whole request should be answered from;
	// only meaningful when UseVision is true.
	Profile   string
	UseVision bool
	// DropImages means the caller should discard the decoded images (mode
	// is "vision" but no usable backend) rather than pass them through to
	// buildUserMessage at all — neither vision nor OCR runs on them.
	DropImages bool
	// Warning, if non-empty, is a note for the model to relay to the user
	// about why an attached image was dropped.
	Warning string
}

// resolveUploadRouting decides how attached images affect an /api/ask
// request's profile, per uploadConfig's documented policy: "ocr" mode
// (the default, or "vision" with no configured backend) never touches
// routing at all — buildUserMessage OCRs the images as plain text.
// "vision" mode reroutes the WHOLE request to VisionProfile, unless the
// same guest/Azure gate resolveAskProfile already enforces for a manual
// profile pick would deny it — in which case the image is dropped with a
// warning instead of failing (or silently upgrading) the question. Pure
// (no I/O), so — like resolveAskProfile itself — this is unit-testable
// without a live chat backend; the browser-based verification a feature
// like this would otherwise need isn't available in every environment
// (e.g. no local LLM/embeddings server), so this is the one place the
// actual policy decision can be pinned down with certainty.
func resolveUploadRouting(hasImages bool, upload uploadConfig, ldap ldapConfig, authActive, hasSession bool, chatProfile string) uploadRouting {
	if !hasImages || upload.ImageMode != "vision" {
		return uploadRouting{}
	}
	if upload.VisionProfile == "" {
		return uploadRouting{DropImages: true, Warning: "Bild-Upload ignoriert: Vision-Modus ist aktiviert, aber kein Vision-Backend ausgewählt (Einstellungen → LLM-Backends & Routing)."}
	}
	if _, deny := resolveAskProfile(upload.VisionProfile, ldap, authActive, hasSession, chatProfile); deny {
		return uploadRouting{DropImages: true, Warning: "Bild-Upload ignoriert: Das konfigurierte Vision-Backend erfordert eine Anmeldung."}
	}
	return uploadRouting{Profile: upload.VisionProfile, UseVision: true}
}

// effectiveUploadImageMode normalizes uploadConfig.ImageMode's "" default
// to "ocr" — the one place that convention is spelled out, so
// handleAuthStatus (surfacing it to the Chat/Agent UI hint) and anything
// else that needs "what will actually happen" rather than "what's
// literally stored" agree with handleAsk's own switch on the same field.
func effectiveUploadImageMode(u uploadConfig) string {
	if u.ImageMode == "vision" {
		return "vision"
	}
	return "ocr"
}

// askImageMaxCount bounds how many attachments a single /api/ask request
// may carry — hardcoded (not an admin setting): every image either becomes
// part of an LLM vision request (cost/latency sensitive) or gets OCR'd
// synchronously inside the same request (tesseract isn't instant either),
// so a per-request cap matters regardless of how large each individual
// attachment is allowed to be (uploadConfig.MaxAttachmentMB below).
const askImageMaxCount = 4

// attachmentMaxMB{Default,Min,Max} bound uploadConfig.MaxAttachmentMB —
// same normalize-and-clamp convention as visionMaxDim{Default,Min,Max}
// above. Default matches this feature's original hardcoded 8 MB, so a
// settings.json predating this field behaves identically to before it
// existed; the 50 MB ceiling matches extract.go's own generic import
// upload caps elsewhere in the codebase.
const (
	attachmentMaxMBDefault = 8
	attachmentMaxMBMin     = 1
	attachmentMaxMBMax     = 50
)

// promptMaxChars{Default,Min,Max} bound uploadConfig.MaxPromptChars — a
// guard against an accidental (or deliberate) huge paste inflating
// retrieval/LLM cost, not against a genuinely long but reasonable
// question. 20000 characters is generous for a real chat question (several
// thousand words) while still catching "pasted an entire document as the
// question" by mistake; the 100000 ceiling leaves room for an admin who
// deliberately wants that use case anyway.
const (
	promptMaxCharsDefault = 20000
	promptMaxCharsMin     = 2000
	promptMaxCharsMax     = 100000
)

// effectiveMaxAttachmentMB/effectiveMaxPromptChars normalize
// uploadConfig's zero value — "not yet configured", including every
// settings.json written before these fields existed — to the built-in
// default, and clamp any explicitly-set value into the allowed range, same
// reasoning as effectiveVisionMaxDim/effectiveVisionJPEGQuality below.
func effectiveMaxAttachmentMB(u uploadConfig) int {
	switch {
	case u.MaxAttachmentMB <= 0:
		return attachmentMaxMBDefault
	case u.MaxAttachmentMB < attachmentMaxMBMin:
		return attachmentMaxMBMin
	case u.MaxAttachmentMB > attachmentMaxMBMax:
		return attachmentMaxMBMax
	default:
		return u.MaxAttachmentMB
	}
}

func effectiveMaxPromptChars(u uploadConfig) int {
	switch {
	case u.MaxPromptChars <= 0:
		return promptMaxCharsDefault
	case u.MaxPromptChars < promptMaxCharsMin:
		return promptMaxCharsMin
	case u.MaxPromptChars > promptMaxCharsMax:
		return promptMaxCharsMax
	default:
		return u.MaxPromptChars
	}
}

// decodedAskImage is one attached file after base64-decoding and
// validation, ready for the vision/OCR path (IsDocument false) or the
// text-extraction path (IsDocument true) below.
type decodedAskImage struct {
	Filename   string
	Data       []byte
	MimeType   string
	IsDocument bool
}

// decodeAskImages base64-decodes and validates every attachment, enforcing
// askImageMaxCount and upload's (admin-configurable) MaxAttachmentMB —
// returns an error (the caller turns it into a 400) on the first violation
// rather than silently dropping or truncating anything the user attached.
// Each file is sniffed and routed: an image/* MIME type takes the existing
// vision/OCR path unchanged; anything else falls back to its filename
// extension, and — only if isExtractableDocument (extract.go) recognizes
// it as a format Import already knows how to read — is accepted as a
// document for buildUserMessage to text-extract instead. Anything matching
// neither is rejected exactly as before this existed.
func decodeAskImages(images []askImageInput, upload uploadConfig) ([]decodedAskImage, error) {
	if len(images) > askImageMaxCount {
		return nil, fmt.Errorf("zu viele Anhänge (max. %d pro Anfrage)", askImageMaxCount)
	}
	maxMB := effectiveMaxAttachmentMB(upload)
	maxBytes := maxMB * 1024 * 1024
	out := make([]decodedAskImage, 0, len(images))
	for _, img := range images {
		data, err := base64.StdEncoding.DecodeString(img.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("Anhang %q: ungültige Daten: %w", img.Filename, err)
		}
		if len(data) == 0 {
			continue
		}
		if len(data) > maxBytes {
			return nil, fmt.Errorf("Anhang %q: zu groß (%.1f MB, Limit %d MB)", img.Filename, float64(len(data))/(1024*1024), maxMB)
		}
		mimeType := http.DetectContentType(data)
		if strings.HasPrefix(mimeType, "image/") {
			out = append(out, decodedAskImage{Filename: img.Filename, Data: data, MimeType: mimeType})
			continue
		}
		ext := fileExtension(img.Filename)
		if !isExtractableDocument(ext) {
			return nil, fmt.Errorf("Anhang %q: weder als Bild noch als unterstütztes Dokument erkannt (%s)", img.Filename, mimeType)
		}
		out = append(out, decodedAskImage{Filename: img.Filename, Data: data, MimeType: mimeType, IsDocument: true})
	}
	return out, nil
}

// visionMaxDim{Default,Min,Max} and visionJPEGQuality{Default,Min,Max}
// bound uploadConfig.VisionMaxDim/VisionJPEGQuality — the admin-facing
// knobs (Settings → LLM-Backends & Routing) for how aggressively
// downscaleForVision shrinks a vision-routed image. Below ~800px a
// scanned document starts visibly losing legibility; above 1600px a
// vision model reads it no better, it just costs more (see
// downscaleForVision's own doc comment). JPEG quality below 50 gets
// blocky enough to fight legibility the same way; above 95 barely
// shrinks the payload at all, defeating the point.
const (
	visionMaxDimDefault = 1600
	visionMaxDimMin     = 800
	visionMaxDimMax     = 1600

	visionJPEGQualityDefault = 85
	visionJPEGQualityMin     = 50
	visionJPEGQualityMax     = 95
)

// effectiveVisionMaxDim/effectiveVisionJPEGQuality normalize
// uploadConfig's zero value — "not yet configured", including every
// settings.json written before these fields existed — to the built-in
// default, and clamp any explicitly-set value into the allowed range.
// The Settings UI already constrains its number inputs to [min,max], but
// a hand-edited settings.json shouldn't be able to request an unbounded
// resize target or JPEG quality.
func effectiveVisionMaxDim(u uploadConfig) int {
	switch {
	case u.VisionMaxDim <= 0:
		return visionMaxDimDefault
	case u.VisionMaxDim < visionMaxDimMin:
		return visionMaxDimMin
	case u.VisionMaxDim > visionMaxDimMax:
		return visionMaxDimMax
	default:
		return u.VisionMaxDim
	}
}

func effectiveVisionJPEGQuality(u uploadConfig) int {
	switch {
	case u.VisionJPEGQuality <= 0:
		return visionJPEGQualityDefault
	case u.VisionJPEGQuality < visionJPEGQualityMin:
		return visionJPEGQualityMin
	case u.VisionJPEGQuality > visionJPEGQualityMax:
		return visionJPEGQualityMax
	default:
		return u.VisionJPEGQuality
	}
}

// downscaleForVision shrinks img so its longest side is at most maxDim,
// re-encoding as JPEG (at the given quality) so the payload actually
// shrinks too (a resized screenshot is still a large lossless PNG
// otherwise). Falls back to returning data/mimeType unchanged whenever
// there's nothing to gain or nothing it can do — already within maxDim,
// or a format stdlib's image package can't decode (e.g. WebP/BMP, which
// still pass decodeAskImages' MIME sniff) — never blocks an upload just
// because it couldn't be optimized.
func downscaleForVision(data []byte, mimeType string, maxDim, quality int) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mimeType
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim || w == 0 || h == 0 {
		return data, mimeType
	}
	scale := float64(maxDim) / float64(w)
	if h > w {
		scale = float64(maxDim) / float64(h)
	}
	newW, newH := int(float64(w)*scale), int(float64(h)*scale)
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	// Simple box-average downsample: each destination pixel is the mean of
	// the source block it covers. No new dependency (stdlib has no image
	// resize) and, for shrinking, noticeably better than nearest-neighbor
	// without needing anything fancier.
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		sy0, sy1 := b.Min.Y+y*h/newH, b.Min.Y+(y+1)*h/newH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < newW; x++ {
			sx0, sx1 := b.Min.X+x*w/newW, b.Min.X+(x+1)*w/newW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint32
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := img.At(sx, sy).RGBA()
					r += pr
					g += pg
					bl += pb
					a += pa
					n++
				}
			}
			dst.Set(x, y, color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return data, mimeType
	}
	return buf.Bytes(), "image/jpeg"
}

// buildUserMessage turns a Chat/Agent question plus any attachments into
// the chatMsg the model actually sees. Documents (decodedAskImage.
// IsDocument) are always text-extracted (extractDocumentText, extract.go)
// and folded into the question as plain text, regardless of the
// vision/OCR split below — a PDF is never sent as a vision image_url
// part, since even a vision-capable backend has no sensible way to "see"
// one. Actual images then follow the pre-existing split: real vision
// content parts when visionCapable, otherwise OCR'd
// (extractImageTextOCR) and folded into the question as plain text like
// documents. Either extraction failing (tesseract/markitdown missing,
// AllowShellExec off, no recognizable content) degrades to a warning
// rather than failing the whole question — the user still gets an
// answer, just without that one attachment's content; the caller is
// expected to surface the returned warnings somehow (handleAsk folds
// them into the system prompt so the model itself can mention them).
func buildUserMessage(question string, images []decodedAskImage, visionCapable bool, ocrCfg importConfig, upload uploadConfig, allowShellExec bool) (chatMsg, []string) {
	if len(images) == 0 {
		return chatMsg{Role: "user", Content: question}, nil
	}

	var docs, imgs []decodedAskImage
	for _, f := range images {
		if f.IsDocument {
			docs = append(docs, f)
		} else {
			imgs = append(imgs, f)
		}
	}

	var warnings []string
	var docText strings.Builder
	for _, doc := range docs {
		text, err := extractDocumentText(doc.Data, doc.Filename, ocrCfg, allowShellExec)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Anhang %q konnte nicht gelesen werden (%v).", doc.Filename, err))
			continue
		}
		fmt.Fprintf(&docText, "--- Inhalt aus Anhang %q ---\n%s\n\n", doc.Filename, text)
	}

	if len(imgs) == 0 {
		content := question
		if docText.Len() > 0 {
			content = docText.String() + "Frage: " + question
		}
		return chatMsg{Role: "user", Content: content}, warnings
	}

	if visionCapable {
		maxDim := effectiveVisionMaxDim(upload)
		quality := effectiveVisionJPEGQuality(upload)
		questionText := question
		if docText.Len() > 0 {
			questionText = docText.String() + "Frage: " + question
		}
		parts := make([]chatContentPart, 0, len(imgs)+1)
		parts = append(parts, chatContentPart{Type: "text", Text: questionText})
		// A filename caption before each image only when there's more than
		// one — with a single attachment it's just noise, but once the
		// model has to juggle several it otherwise has no way to say "the
		// second image" back to the user.
		labelImages := len(imgs) > 1
		for _, img := range imgs {
			data, mimeType := downscaleForVision(img.Data, img.MimeType, maxDim, quality)
			if labelImages {
				parts = append(parts, chatContentPart{Type: "text", Text: fmt.Sprintf("Bild %q:", img.Filename)})
			}
			parts = append(parts, chatContentPart{
				Type: "image_url",
				ImageURL: &chatImageURL{
					URL: fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data)),
				},
			})
		}
		return chatMsg{Role: "user", Parts: parts}, warnings
	}

	// Continue writing into docText (a strings.Builder) rather than
	// starting a fresh one — it must not be copied by value after already
	// being written to (panics: "illegal use of non-zero Builder copied by
	// value").
	for _, img := range imgs {
		text, err := extractImageTextOCR(img.Data, img.Filename, ocrCfg, allowShellExec)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Bild %q konnte nicht per OCR gelesen werden (%v).", img.Filename, err))
			continue
		}
		fmt.Fprintf(&docText, "--- Text aus Bild %q (per OCR erkannt) ---\n%s\n\n", img.Filename, text)
	}
	content := question
	if docText.Len() > 0 {
		content = docText.String() + "Frage: " + question
	}
	return chatMsg{Role: "user", Content: content}, warnings
}
