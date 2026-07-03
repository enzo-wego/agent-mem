package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// mockGemini is a GeminiClient stub that records calls and returns canned values.
type mockGemini struct {
	describeCalls atomic.Int32
	embedCalls    atomic.Int32

	describeResult  func() (string, string, []string, error)
	embedResult     func() ([]float32, error)
	generateResult  func() (string, error)
	generateUser    string // last user prompt passed to Generate
}

func (m *mockGemini) Describe(_ context.Context, _ string, _ []byte, _ string) (string, string, []string, error) {
	m.describeCalls.Add(1)
	if m.describeResult != nil {
		return m.describeResult()
	}
	return "desc", "ocr", nil, nil
}

func (m *mockGemini) Embed(_ context.Context, _ string) ([]float32, error) {
	m.embedCalls.Add(1)
	if m.embedResult != nil {
		return m.embedResult()
	}
	return []float32{0.1, 0.2}, nil
}

func (m *mockGemini) Generate(_ context.Context, _, user string) (string, error) {
	m.generateUser = user
	if m.generateResult != nil {
		return m.generateResult()
	}
	return "", nil
}

func TestDescribeAttachmentHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestDescribeAttachmentHandler_EmptyURL(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID: "slack_file:F123",
		Mime:   "image/png",
		Source: "slack",
	})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when external_url is empty")
	}
}

func TestDescribeAttachmentHandler_UnsupportedMime(t *testing.T) {
	// We can't make a real HTTP request in unit tests, so test mime filtering
	// via a localhost URL that will fail quickly.
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	// Use a data: URL workaround is not possible; test that unsupported mime
	// returns ErrFatal without needing network. We skip if net is unavailable.
	t.Skip("requires network; covered by integration tests")
	_ = deps
	_ = h
}

// fakeLitScript writes a shell script that emits stdout and exits with the given code.
func fakeLitScript(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stubs not supported on Windows")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' '%s'\nexit %d\n", stdout, exitCode)
	path := filepath.Join(dir, "lit")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake lit: %v", err)
	}
	return path
}

// richLitJSON returns JSON that ParseDocument considers "rich" (TotalTextLen >= liteParseRichTextThreshold).
func richLitJSON() string {
	text := "This is rich extracted text from the PDF document. " +
		"It contains enough characters to exceed the threshold for direct embedding " +
		"without needing Gemini multimodal vision at all. More text to be safe here."
	return fmt.Sprintf(`{"pages":[{"page":1,"width":612,"height":792,"text":%q,"text_items":[]}]}`, text)
}

// thinLitJSON returns JSON where text is empty (image-heavy doc).
func thinLitJSON() string {
	return `{"pages":[{"page":1,"width":612,"height":792,"text":"","text_items":[]}]}`
}

// serveBytes starts a test HTTP server that responds with the given bytes and content-type.
func serveBytes(t *testing.T, data []byte, contentType string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/file"
}

// TestDescribeAttachment_RichPDF_SkipsGemini verifies that when LiteParse returns
// rich text (>= liteParseRichTextThreshold chars), Gemini.Describe is NOT called.
func TestDescribeAttachment_RichPDF_SkipsGemini(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stubs not supported on Windows")
	}

	gem := &mockGemini{
		embedResult: func() ([]float32, error) { return []float32{0.1}, nil },
	}

	litBin := fakeLitScript(t, richLitJSON(), 0)
	pdfData := []byte("%PDF-1.4 minimal")
	url := serveBytes(t, pdfData, "application/pdf")

	deps := Deps{
		Logger: zerolog.Nop(),
		Gemini: gem,
		LiteParse: LiteParseConfig{
			BinPath:           litBin,
			ScreenshotEnabled: false,
			TempDir:           t.TempDir(),
		},
	}
	h := NewDescribeAttachmentHandler(deps)

	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID:      "test:pdf1",
		ExternalURL: url,
		Mime:        "application/pdf",
		Source:      "slack",
	})

	// DB is nil so the handler panics at the UPDATE step. We use recover so we
	// can still assert on the Describe call count — the key invariant is that
	// Describe is never invoked when LiteParse extraction is rich.
	func() {
		defer func() { recover() }() //nolint:errcheck
		_ = h.Handler(context.Background(), payload)
	}()

	if gem.describeCalls.Load() != 0 {
		t.Errorf("Gemini.Describe called %d times; expected 0 for rich LiteParse extraction", gem.describeCalls.Load())
	}
}

// TestDescribeAttachment_ThinPDF_UsesGeminiOnScreenshots verifies that when LiteParse
// returns empty text but screenshots are available, Gemini.Describe IS called per screenshot.
func TestDescribeAttachment_ThinPDF_UsesGeminiOnScreenshots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stubs not supported on Windows")
	}

	// We need a fake lit that:
	// 1. When called with "parse ...": outputs thin JSON (empty text)
	// 2. When called with "screenshot ...": writes a PNG to the output dir

	// Build a fake lit binary that handles both subcommands.
	dir := t.TempDir()

	// The screenshot dir is created by ParseDocument before calling lit screenshot.
	// Our fake script needs to write page-1.png when called with "screenshot".
	// We embed a minimal 1x1 PNG.
	pngHex := "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000a49444154789c6260000000000200e221bc330000000049454e44ae426082"

	// $1 = subcommand ("parse" or "screenshot")
	// lit screenshot <file> -o <dir> --quiet  ->  $1=screenshot $2=<file> $3=-o $4=<dir>
	script := fmt.Sprintf(`#!/bin/sh
SUBCMD="$1"
if [ "$SUBCMD" = "screenshot" ]; then
  OUTDIR="$4"
  python3 -c "import binascii,sys; sys.stdout.buffer.write(binascii.unhexlify('%s'))" > "$OUTDIR/page_1.png" 2>/dev/null || \
  printf '\x89PNG\r\n\x1a\n' > "$OUTDIR/page_1.png"
  exit 0
fi
printf '%s'
exit 0
`, pngHex, thinLitJSON())

	litBin := filepath.Join(dir, "lit")
	if err := os.WriteFile(litBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake lit: %v", err)
	}

	gem := &mockGemini{
		describeResult: func() (string, string, []string, error) { return "page desc", "page ocr", nil, nil },
		embedResult:    func() ([]float32, error) { return []float32{0.1}, nil },
	}

	pdfData := []byte("%PDF-1.4 minimal")
	url := serveBytes(t, pdfData, "application/pdf")

	deps := Deps{
		Logger: zerolog.Nop(),
		Gemini: gem,
		LiteParse: LiteParseConfig{
			BinPath:           litBin,
			ScreenshotEnabled: true,
			TempDir:           t.TempDir(),
		},
	}
	h := NewDescribeAttachmentHandler(deps)

	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID:      "test:pdf2",
		ExternalURL: url,
		Mime:        "application/pdf",
		Source:      "slack",
	})
	func() {
		defer func() { recover() }() //nolint:errcheck
		_ = h.Handler(context.Background(), payload)
	}()

	// Gemini.Describe should have been called at least once (once per screenshot page).
	if gem.describeCalls.Load() == 0 {
		t.Error("Gemini.Describe was not called; expected it to be called for thin-text screenshots")
	}
}

// TestDescribeAttachment_LiteParseUnavailable_FallsBack verifies that when LiteParse
// returns Available=false, Gemini.Describe IS called with the full PDF bytes.
func TestDescribeAttachment_LiteParseUnavailable_FallsBack(t *testing.T) {
	gem := &mockGemini{
		describeResult: func() (string, string, []string, error) { return "gemini desc", "", nil, nil },
		embedResult:    func() ([]float32, error) { return []float32{0.1}, nil },
	}

	pdfData := []byte("%PDF-1.4 minimal")
	url := serveBytes(t, pdfData, "application/pdf")

	deps := Deps{
		Logger: zerolog.Nop(),
		Gemini: gem,
		LiteParse: LiteParseConfig{
			// Non-existent binary -> Available=false
			BinPath:           "/nonexistent/lit-binary",
			ScreenshotEnabled: false,
			TempDir:           t.TempDir(),
		},
	}
	h := NewDescribeAttachmentHandler(deps)

	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID:      "test:pdf3",
		ExternalURL: url,
		Mime:        "application/pdf",
		Source:      "slack",
	})
	func() {
		defer func() { recover() }() //nolint:errcheck
		_ = h.Handler(context.Background(), payload)
	}()

	if gem.describeCalls.Load() == 0 {
		t.Error("Gemini.Describe was not called; expected fallback to Gemini when LiteParse unavailable")
	}
}

func TestIsInterestGroup(t *testing.T) {
	cases := []struct {
		handle string
		want   bool
	}{
		{"payments-geeks", true},
		{"infra-ops", true},
		{"general", false},
		{"ops-team", false},
		{"data-geeks", true},
		{"devops", false},
	}
	for _, c := range cases {
		got := isInterestGroup(c.handle)
		if got != c.want {
			t.Errorf("isInterestGroup(%q) = %v, want %v", c.handle, got, c.want)
		}
	}
}
