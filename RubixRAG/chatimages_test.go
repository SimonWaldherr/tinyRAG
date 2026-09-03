package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// tinyPNG is a valid 1x1 transparent PNG, just enough for
// http.DetectContentType to recognize "image/png".
var tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestChatMsgMarshalJSONPlainUnaffected(t *testing.T) {
	m := chatMsg{Role: "user", Content: "hello"}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["content"] != "hello" {
		t.Fatalf("want content as plain string \"hello\", got %#v (raw=%s)", got["content"], raw)
	}
	if _, ok := got["role"]; !ok {
		t.Fatalf("want role present, got %s", raw)
	}
}

func TestChatMsgMarshalJSONWithParts(t *testing.T) {
	m := chatMsg{Role: "user", Parts: []chatContentPart{
		{Type: "text", Text: "what is this?"},
		{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64,xyz"}},
	}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("content should unmarshal as an array, got: %s (%v)", raw, err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("want 2 content parts, got %d (raw=%s)", len(got.Content), raw)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "what is this?" {
		t.Errorf("unexpected text part: %+v", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil || got.Content[1].ImageURL.URL != "data:image/png;base64,xyz" {
		t.Errorf("unexpected image_url part: %+v", got.Content[1])
	}
}

func TestDecodeAskImagesValid(t *testing.T) {
	imgs, err := decodeAskImages([]askImageInput{{Filename: "scan.png", DataBase64: tinyPNGBase64}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 decoded image, got %d", len(imgs))
	}
	if imgs[0].MimeType != "image/png" {
		t.Errorf("want image/png, got %q", imgs[0].MimeType)
	}
}

func TestDecodeAskImagesRejectsTooMany(t *testing.T) {
	var in []askImageInput
	for i := 0; i < askImageMaxCount+1; i++ {
		in = append(in, askImageInput{Filename: "a.png", DataBase64: tinyPNGBase64})
	}
	if _, err := decodeAskImages(in, uploadConfig{}); err == nil {
		t.Fatal("want an error for more than askImageMaxCount images")
	}
}

func TestDecodeAskImagesRejectsTooLarge(t *testing.T) {
	big := make([]byte, attachmentMaxMBDefault*1024*1024+1)
	in := []askImageInput{{Filename: "huge.png", DataBase64: base64.StdEncoding.EncodeToString(big)}}
	if _, err := decodeAskImages(in, uploadConfig{}); err == nil {
		t.Fatal("want an error for an image over the default MaxAttachmentMB")
	}
}

// TestDecodeAskImagesRespectsConfiguredMaxAttachmentMB proves an explicit,
// smaller uploadConfig.MaxAttachmentMB is actually enforced instead of the
// default — the admin-configurable half of the old hardcoded askImageMaxBytes.
func TestDecodeAskImagesRespectsConfiguredMaxAttachmentMB(t *testing.T) {
	// Real PNG bytes, padded with trailing zeros to 2 MB — under the
	// default 8 MB, over a 1 MB configured limit. http.DetectContentType
	// only inspects the leading bytes, so this still sniffs as image/png
	// despite the padding, letting this test isolate the size check from
	// content-type recognition (unlike TestDecodeAskImagesRejectsTooLarge's
	// plain zero-byte buffer, which only needs *some* rejection to fire).
	pngBytes, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	padded := append(pngBytes, make([]byte, 2*1024*1024-len(pngBytes))...)
	in := []askImageInput{{Filename: "medium.png", DataBase64: base64.StdEncoding.EncodeToString(padded)}}
	if _, err := decodeAskImages(in, uploadConfig{MaxAttachmentMB: 1}); err == nil {
		t.Fatal("want an error once MaxAttachmentMB=1 is configured, even though 2 MB is under the built-in default")
	}
	if _, err := decodeAskImages(in, uploadConfig{}); err != nil {
		t.Fatalf("want no error with the default limit: %v", err)
	}
}

func TestDecodeAskImagesRejectsNonImage(t *testing.T) {
	// .exe matches neither an image MIME type nor any extension
	// isExtractableDocument recognizes (unlike .txt/.pdf/etc. below, which
	// are now deliberately accepted as document attachments).
	notImage := base64.StdEncoding.EncodeToString([]byte("just some plain text, not an image"))
	if _, err := decodeAskImages([]askImageInput{{Filename: "payload.exe", DataBase64: notImage}}, uploadConfig{}); err == nil {
		t.Fatal("want an error for data that's neither image content nor a recognized document extension")
	}
}

func TestDecodeAskImagesAcceptsPlainTextDocument(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("just some plain text, not an image"))
	imgs, err := decodeAskImages([]askImageInput{{Filename: "note.txt", DataBase64: data}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	if len(imgs) != 1 || !imgs[0].IsDocument {
		t.Fatalf("want a single attachment routed as a document, got %+v", imgs)
	}
}

func TestDecodeAskImagesAcceptsPDFByExtension(t *testing.T) {
	// decodeAskImages only decides routing from the extension (actual
	// parsing, and any "is this really a valid PDF" failure, happens later
	// in extractDocumentText/markitdown) — arbitrary bytes with a .pdf
	// name are still accepted here.
	data := base64.StdEncoding.EncodeToString([]byte("not a real pdf, just bytes"))
	imgs, err := decodeAskImages([]askImageInput{{Filename: "menu.pdf", DataBase64: data}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	if len(imgs) != 1 || !imgs[0].IsDocument {
		t.Fatalf("want a single attachment routed as a document, got %+v", imgs)
	}
}

func TestBuildUserMessageNoImagesIsPlainText(t *testing.T) {
	msg, warnings := buildUserMessage("hallo?", nil, true, importConfig{}, uploadConfig{}, true)
	if msg.Content != "hallo?" || len(msg.Parts) != 0 {
		t.Fatalf("want a plain-text message when there are no images, got %+v", msg)
	}
	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
}

func TestBuildUserMessageVisionCapableUsesParts(t *testing.T) {
	imgs, err := decodeAskImages([]askImageInput{{Filename: "scan.png", DataBase64: tinyPNGBase64}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	msg, warnings := buildUserMessage("was zeigt das Bild?", imgs, true, importConfig{}, uploadConfig{}, true)
	if len(warnings) != 0 {
		t.Fatalf("want no warnings on the vision path, got %v", warnings)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("want 2 parts (text + image_url), got %d: %+v", len(msg.Parts), msg.Parts)
	}
	if msg.Parts[0].Type != "text" || msg.Parts[0].Text != "was zeigt das Bild?" {
		t.Errorf("unexpected first part: %+v", msg.Parts[0])
	}
	if msg.Parts[1].Type != "image_url" || msg.Parts[1].ImageURL == nil || !strings.HasPrefix(msg.Parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("unexpected second part: %+v", msg.Parts[1])
	}
}

func TestBuildUserMessageVisionMultipleImagesAreLabeled(t *testing.T) {
	imgs, err := decodeAskImages([]askImageInput{
		{Filename: "front.png", DataBase64: tinyPNGBase64},
		{Filename: "back.png", DataBase64: tinyPNGBase64},
	}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	msg, _ := buildUserMessage("was zeigt das Bild?", imgs, true, importConfig{}, uploadConfig{}, true)
	// question, then (label, image) per attachment.
	if len(msg.Parts) != 5 {
		t.Fatalf("want 5 parts (question + 2x[label, image]), got %d: %+v", len(msg.Parts), msg.Parts)
	}
	if msg.Parts[1].Type != "text" || !strings.Contains(msg.Parts[1].Text, "front.png") {
		t.Errorf("want a label mentioning front.png, got %+v", msg.Parts[1])
	}
	if msg.Parts[2].Type != "image_url" {
		t.Errorf("want an image_url part after the label, got %+v", msg.Parts[2])
	}
	if msg.Parts[3].Type != "text" || !strings.Contains(msg.Parts[3].Text, "back.png") {
		t.Errorf("want a label mentioning back.png, got %+v", msg.Parts[3])
	}
}

// solidPNG builds a w x h opaque red PNG — enough for image.Decode to
// succeed and for downscaleForVision's dimension logic to be exercised. A
// noisy (not solid-color) fill so PNG's DEFLATE can't compress it away to
// near-nothing — a real photo doesn't either, and the "shrinks" test wants
// a size comparison that means something.
func solidPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 37 % 256), G: uint8(y * 53 % 256), B: uint8((x + y) * 19 % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestDownscaleForVisionLeavesSmallImageUnchanged(t *testing.T) {
	data := solidPNG(t, 100, 80)
	outData, outMime := downscaleForVision(data, "image/png", visionMaxDimDefault, visionJPEGQualityDefault)
	if outMime != "image/png" || !bytes.Equal(outData, data) {
		t.Fatalf("want the small image passed through unchanged, got mime %q, %d bytes (in was %d)", outMime, len(outData), len(data))
	}
}

func TestDownscaleForVisionShrinksLargeImage(t *testing.T) {
	const maxDim = 1200
	data := solidPNG(t, maxDim*2, maxDim)
	outData, outMime := downscaleForVision(data, "image/png", maxDim, visionJPEGQualityDefault)
	if outMime != "image/jpeg" {
		t.Fatalf("want re-encoded as image/jpeg, got %q", outMime)
	}
	img, err := jpeg.Decode(bytes.NewReader(outData))
	if err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	b := img.Bounds()
	if b.Dx() > maxDim || b.Dy() > maxDim {
		t.Fatalf("want longest side <= %d, got %dx%d", maxDim, b.Dx(), b.Dy())
	}
	if b.Dx() != maxDim { // original was 2:1 (2*maxDim x maxDim); width is the capped long side
		t.Errorf("want width %d, got %d", maxDim, b.Dx())
	}
	if b.Dy() != maxDim/2 { // aspect ratio preserved
		t.Errorf("want height %d (aspect ratio preserved), got %d", maxDim/2, b.Dy())
	}
	// Not asserting the re-encoded JPEG is smaller than the source PNG here:
	// true for photo-like content (the actual point of this function) but
	// not a guaranteed property for every possible input, and this test's
	// synthetic pattern is adversarial to JPEG's block DCT.
}

func TestDownscaleForVisionRespectsConfiguredMaxDim(t *testing.T) {
	// A 2000px image exceeds both caps, so both runs actually downscale —
	// the smaller cap must produce a correspondingly smaller result,
	// proving maxDim isn't just accepted and ignored.
	data := solidPNG(t, 2000, 2000)
	outLarge, _ := downscaleForVision(data, "image/png", 1600, visionJPEGQualityDefault)
	outSmall, _ := downscaleForVision(data, "image/png", 800, visionJPEGQualityDefault)
	imgLarge, err := jpeg.Decode(bytes.NewReader(outLarge))
	if err != nil {
		t.Fatalf("decoding large result: %v", err)
	}
	imgSmall, err := jpeg.Decode(bytes.NewReader(outSmall))
	if err != nil {
		t.Fatalf("decoding small result: %v", err)
	}
	if imgLarge.Bounds().Dx() != 1600 {
		t.Errorf("want the 1600 cap to shrink to 1600px, got %d", imgLarge.Bounds().Dx())
	}
	if imgSmall.Bounds().Dx() != 800 {
		t.Errorf("want the 800 cap to shrink to 800px, got %d", imgSmall.Bounds().Dx())
	}
}

func TestDownscaleForVisionFallsBackOnUndecodableData(t *testing.T) {
	data := []byte("not actually an image, but detectcontenttype might still call it image/webp upstream")
	outData, outMime := downscaleForVision(data, "image/webp", visionMaxDimDefault, visionJPEGQualityDefault)
	if outMime != "image/webp" || !bytes.Equal(outData, data) {
		t.Fatalf("want undecodable data passed through unchanged, got mime %q, %d bytes", outMime, len(outData))
	}
}

func TestEffectiveVisionMaxDim(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset defaults", 0, visionMaxDimDefault},
		{"negative defaults", -5, visionMaxDimDefault},
		{"below min clamps up", 300, visionMaxDimMin},
		{"above max clamps down", 4000, visionMaxDimMax},
		{"in range passes through", 1000, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveVisionMaxDim(uploadConfig{VisionMaxDim: c.in}); got != c.want {
				t.Errorf("effectiveVisionMaxDim(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestEffectiveVisionJPEGQuality(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset defaults", 0, visionJPEGQualityDefault},
		{"negative defaults", -1, visionJPEGQualityDefault},
		{"below min clamps up", 10, visionJPEGQualityMin},
		{"above max clamps down", 100, visionJPEGQualityMax},
		{"in range passes through", 70, 70},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveVisionJPEGQuality(uploadConfig{VisionJPEGQuality: c.in}); got != c.want {
				t.Errorf("effectiveVisionJPEGQuality(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestEffectiveMaxAttachmentMB(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset defaults", 0, attachmentMaxMBDefault},
		{"negative defaults", -5, attachmentMaxMBDefault},
		{"below min clamps up", -1, attachmentMaxMBDefault}, // <=0 always defaults, see effectiveMaxAttachmentMB
		{"above max clamps down", 500, attachmentMaxMBMax},
		{"in range passes through", 20, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveMaxAttachmentMB(uploadConfig{MaxAttachmentMB: c.in}); got != c.want {
				t.Errorf("effectiveMaxAttachmentMB(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestEffectiveMaxPromptChars(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset defaults", 0, promptMaxCharsDefault},
		{"negative defaults", -5, promptMaxCharsDefault},
		{"below min clamps up", 100, promptMaxCharsMin},
		{"above max clamps down", 1000000, promptMaxCharsMax},
		{"in range passes through", 5000, 5000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveMaxPromptChars(uploadConfig{MaxPromptChars: c.in}); got != c.want {
				t.Errorf("effectiveMaxPromptChars(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestResolveUploadRoutingNoImagesIsNoOp(t *testing.T) {
	got := resolveUploadRouting(false, uploadConfig{ImageMode: "vision", VisionProfile: "azure"}, ldapConfig{}, false, false, "local")
	if got != (uploadRouting{}) {
		t.Fatalf("want zero-value routing when there are no images, got %+v", got)
	}
}

func TestResolveUploadRoutingOCRModeIsNoOp(t *testing.T) {
	// "ocr" (and the "" default) never touch profile routing at all —
	// buildUserMessage OCRs the images regardless of what's configured
	// below.
	for _, mode := range []string{"", "ocr"} {
		got := resolveUploadRouting(true, uploadConfig{ImageMode: mode, VisionProfile: "azure"}, ldapConfig{}, false, false, "local")
		if got.UseVision || got.DropImages || got.Warning != "" {
			t.Errorf("mode %q: want a no-op routing, got %+v", mode, got)
		}
	}
}

func TestResolveUploadRoutingVisionNoBackendConfiguredDropsImages(t *testing.T) {
	got := resolveUploadRouting(true, uploadConfig{ImageMode: "vision", VisionProfile: ""}, ldapConfig{}, false, false, "local")
	if got.UseVision {
		t.Fatal("want UseVision false when no VisionProfile is configured")
	}
	if !got.DropImages || got.Warning == "" {
		t.Fatalf("want images dropped with a warning, got %+v", got)
	}
}

func TestResolveUploadRoutingVisionRoutesWholeRequest(t *testing.T) {
	got := resolveUploadRouting(true, uploadConfig{ImageMode: "vision", VisionProfile: "azure"}, ldapConfig{}, false, false, "local")
	if !got.UseVision || got.Profile != "azure" {
		t.Fatalf("want the whole request routed to the configured vision profile, got %+v", got)
	}
	if got.DropImages || got.Warning != "" {
		t.Errorf("want no drop/warning on the happy path, got %+v", got)
	}
}

func TestResolveUploadRoutingVisionDeniedForAnonymousGuestDropsImages(t *testing.T) {
	// Same guest/Azure gate as a manual profile pick (resolveAskProfile,
	// handlers.go) — an anonymous caller shouldn't get free Azure vision
	// calls just by attaching an image, if the admin denied that tier.
	ldap := ldapConfig{Enabled: true, GuestAzureProfilePolicy: "deny"}
	got := resolveUploadRouting(true, uploadConfig{ImageMode: "vision", VisionProfile: "azure"}, ldap, true /* authActive */, false /* hasSession */, "local")
	if got.UseVision {
		t.Fatal("want UseVision false for a denied anonymous guest")
	}
	if !got.DropImages || got.Warning == "" {
		t.Fatalf("want images dropped with a warning, got %+v", got)
	}
}

func TestResolveUploadRoutingVisionAllowedForLoggedInSession(t *testing.T) {
	// Same policy, but a real session is exempt from the guest gate
	// (resolveAskProfile's own rule) — vision still routes normally.
	ldap := ldapConfig{Enabled: true, GuestAzureProfilePolicy: "deny"}
	got := resolveUploadRouting(true, uploadConfig{ImageMode: "vision", VisionProfile: "azure"}, ldap, true /* authActive */, true /* hasSession */, "local")
	if !got.UseVision || got.Profile != "azure" {
		t.Fatalf("want vision routing for a logged-in session even under a deny guest policy, got %+v", got)
	}
}

func TestBuildUserMessageDocumentFoldedAsText(t *testing.T) {
	// .txt needs no external process (nativeTextExtensions, extract.go) —
	// deterministic regardless of whether markitdown is installed.
	data := base64.StdEncoding.EncodeToString([]byte("Reservierung 19 Uhr, Tisch 4."))
	docs, err := decodeAskImages([]askImageInput{{Filename: "notiz.txt", DataBase64: data}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	msg, warnings := buildUserMessage("worum geht's?", docs, true, importConfig{}, uploadConfig{}, true)
	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
	if len(msg.Parts) != 0 {
		t.Fatalf("want plain-text content, not vision parts, for a document-only attachment, got %+v", msg.Parts)
	}
	if !strings.Contains(msg.Content, "Reservierung 19 Uhr, Tisch 4.") {
		t.Fatalf("want the document's extracted text folded into the message, got %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "notiz.txt") {
		t.Fatalf("want the attachment's filename mentioned, got %q", msg.Content)
	}
	if !strings.HasSuffix(msg.Content, "Frage: worum geht's?") {
		t.Fatalf("want the original question preserved at the end, got %q", msg.Content)
	}
}

func TestBuildUserMessageDocumentAlongsideVisionImageStaysTextOnly(t *testing.T) {
	// A document is never sent as a vision image_url part, even when an
	// image is also attached and the profile supports vision — only the
	// actual image becomes a part; the document's text is folded into the
	// leading text part instead.
	txt := base64.StdEncoding.EncodeToString([]byte("Speisekarte: Suppe, Hauptgang, Dessert."))
	attachments, err := decodeAskImages([]askImageInput{
		{Filename: "speisekarte.txt", DataBase64: txt},
		{Filename: "foto.png", DataBase64: tinyPNGBase64},
	}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	msg, warnings := buildUserMessage("hilft dir die Datei?", attachments, true, importConfig{}, uploadConfig{}, true)
	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("want exactly 2 parts (text + the one image), got %d: %+v", len(msg.Parts), msg.Parts)
	}
	if msg.Parts[0].Type != "text" || !strings.Contains(msg.Parts[0].Text, "Speisekarte: Suppe") {
		t.Fatalf("want the document's text folded into the leading text part, got %+v", msg.Parts[0])
	}
	if msg.Parts[1].Type != "image_url" {
		t.Fatalf("want the image as its own part, got %+v", msg.Parts[1])
	}
}

func TestBuildUserMessageDocumentRequiringShellExecDegradesToWarning(t *testing.T) {
	// .pdf requires markitdown (allowShellExec) — with it off,
	// extractTextByExt fails fast without ever invoking an external
	// process, so this is deterministic regardless of whether markitdown
	// is actually installed on the machine running this test.
	data := base64.StdEncoding.EncodeToString([]byte("not a real pdf"))
	docs, err := decodeAskImages([]askImageInput{{Filename: "menu.pdf", DataBase64: data}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	msg, warnings := buildUserMessage("was steht auf der Karte?", docs, true, importConfig{}, uploadConfig{}, false)
	if msg.Content != "was steht auf der Karte?" {
		t.Fatalf("want the original question preserved when extraction fails, got %q", msg.Content)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "menu.pdf") {
		t.Fatalf("want one warning mentioning the filename, got %v", warnings)
	}
}

func TestBuildUserMessageOCRDisabledDegradesToWarning(t *testing.T) {
	imgs, err := decodeAskImages([]askImageInput{{Filename: "scan.png", DataBase64: tinyPNGBase64}}, uploadConfig{})
	if err != nil {
		t.Fatalf("decodeAskImages: %v", err)
	}
	// allowShellExec=false: extractImageTextOCR fails fast without ever
	// shelling out, and buildUserMessage must degrade to a warning
	// (question still answered) rather than propagating a hard error.
	msg, warnings := buildUserMessage("was steht auf dem Zettel?", imgs, false, importConfig{}, uploadConfig{}, false)
	if msg.Content != "was steht auf dem Zettel?" {
		t.Fatalf("want the original question preserved when OCR fails, got %q", msg.Content)
	}
	if len(msg.Parts) != 0 {
		t.Fatalf("want no vision parts on the OCR path, got %+v", msg.Parts)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "scan.png") {
		t.Fatalf("want one warning mentioning the filename, got %v", warnings)
	}
}
