package handlers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rs/zerolog"
)

// TestParseDocument_BinaryMissing asserts Available=false and nil error when
// the lit binary does not exist.
func TestParseDocument_BinaryMissing(t *testing.T) {
	cfg := LiteParseConfig{
		BinPath: "/nonexistent/path/to/lit-binary-that-does-not-exist",
	}
	result, err := ParseDocument(context.Background(), cfg, []byte("%PDF-1.4"), "application/pdf", zerolog.Nop())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if result.Available {
		t.Fatal("expected Available=false for missing binary")
	}
	if result.FailureReason == "" {
		t.Fatal("expected non-empty FailureReason")
	}
}

// TestParseDocument_NonZeroExit asserts Available=false when the binary exits non-zero.
func TestParseDocument_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	// Write a fake "lit" script that always exits 1.
	dir := t.TempDir()
	fakeScript := filepath.Join(dir, "lit")
	if err := os.WriteFile(fakeScript, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}

	cfg := LiteParseConfig{
		BinPath: fakeScript,
		TempDir: t.TempDir(),
	}
	result, err := ParseDocument(context.Background(), cfg, []byte("%PDF-1.4"), "application/pdf", zerolog.Nop())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if result.Available {
		t.Fatal("expected Available=false for non-zero exit")
	}
	if result.FailureReason == "" {
		t.Fatal("expected non-empty FailureReason")
	}
}

// TestParseDocument_JSONParseFailure asserts Available=false when the binary
// outputs invalid JSON.
func TestParseDocument_JSONParseFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	dir := t.TempDir()
	fakeScript := filepath.Join(dir, "lit")
	script := "#!/bin/sh\nprintf 'this is not json'\n"
	if err := os.WriteFile(fakeScript, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}

	cfg := LiteParseConfig{
		BinPath: fakeScript,
		TempDir: t.TempDir(),
	}
	result, err := ParseDocument(context.Background(), cfg, []byte("%PDF-1.4"), "application/pdf", zerolog.Nop())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if result.Available {
		t.Fatal("expected Available=false for invalid JSON output")
	}
	if result.FailureReason == "" {
		t.Fatal("expected non-empty FailureReason")
	}
}

// TestParseDocument_RichPDF runs against the real lit binary (skipped if not in PATH).
// It generates a minimal valid PDF inline and asserts TotalTextLen > 0.
func TestParseDocument_RichPDF(t *testing.T) {
	if _, err := exec.LookPath("lit"); err != nil {
		t.Skip("lit binary not in PATH; skipping real PDF test")
	}

	// Minimal valid single-page PDF with the text "Hello World".
	pdfBytes := minimalPDF("Hello World this is a test document with enough text to be parsed correctly by liteparse.")

	cfg := LiteParseConfig{
		BinPath:           "lit",
		ScreenshotEnabled: false,
		TempDir:           t.TempDir(),
	}
	result, err := ParseDocument(context.Background(), cfg, pdfBytes, "application/pdf", zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Available {
		t.Fatalf("expected Available=true, FailureReason: %s", result.FailureReason)
	}
	if result.TotalTextLen == 0 {
		t.Fatal("expected TotalTextLen > 0 for a text PDF")
	}
}

// TestCombinePageTexts verifies the helper joins texts with double newlines.
func TestCombinePageTexts(t *testing.T) {
	pages := []LiteParsePage{
		{PageNumber: 1, Text: "Page one"},
		{PageNumber: 2, Text: "Page two"},
		{PageNumber: 3, Text: "Page three"},
	}
	got := combinePageTexts(pages)
	want := "Page one\n\nPage two\n\nPage three"
	if got != want {
		t.Errorf("combinePageTexts = %q, want %q", got, want)
	}
}

func TestCombinePageTexts_Empty(t *testing.T) {
	got := combinePageTexts(nil)
	if got != "" {
		t.Errorf("combinePageTexts(nil) = %q, want empty", got)
	}
}

// TestParseDocument_ValidJSON asserts correct parsing when the fake binary returns
// well-formed JSON matching the lit output schema.
func TestParseDocument_ValidJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	dir := t.TempDir()
	fakeScript := filepath.Join(dir, "lit")
	jsonOut := `{"pages":[{"page":1,"width":612,"height":792,"text":"Hello from fake lit","text_items":[]}]}`
	script := fmt.Sprintf("#!/bin/sh\nprintf '%s'\n", jsonOut)
	if err := os.WriteFile(fakeScript, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}

	cfg := LiteParseConfig{
		BinPath:           fakeScript,
		ScreenshotEnabled: false,
		TempDir:           t.TempDir(),
	}
	result, err := ParseDocument(context.Background(), cfg, []byte("%PDF-1.4"), "application/pdf", zerolog.Nop())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if !result.Available {
		t.Fatalf("expected Available=true, got FailureReason: %s", result.FailureReason)
	}
	if len(result.Pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(result.Pages))
	}
	if result.Pages[0].Text != "Hello from fake lit" {
		t.Errorf("unexpected page text: %q", result.Pages[0].Text)
	}
	if result.TotalTextLen != len("Hello from fake lit") {
		t.Errorf("TotalTextLen = %d, want %d", result.TotalTextLen, len("Hello from fake lit"))
	}
}

// minimalPDF returns a minimal valid PDF byte slice containing the given text.
// This is a hand-crafted PDF without a proper cross-reference table offset;
// sufficient for testing that lit accepts it but text extraction accuracy is not guaranteed.
func minimalPDF(text string) []byte {
	// Use a pre-built minimal PDF that liteparse can actually parse.
	// This is a 1-page PDF with the text embedded as a stream.
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	pdf := fmt.Sprintf(`%%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/MediaBox[0 0 612 792]/Parent 2 0 R/Resources<</Font<</F1<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>>>>>/Contents 4 0 R>>endobj
4 0 obj<</Length %d>>
stream
%s
endstream
endobj
xref
0 5
0000000000 65535 f
0000000009 00000 n
0000000058 00000 n
0000000115 00000 n
0000000274 00000 n
trailer<</Size 5/Root 1 0 R>>
startxref
0
%%%%EOF`, len(stream)+1, stream)
	return []byte(pdf)
}
