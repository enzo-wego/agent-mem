package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// mockGemini is a GeminiClient stub that records calls and returns canned values.
type mockGemini struct {
	describeCalls atomic.Int32
	embedCalls    atomic.Int32

	describeResult      func() (string, string, []string, error)
	embedResult         func() ([]float32, error)
	generateResult      func() (string, error)
	cheapGenerateResult func() (string, error)
	cheapGenerateCalls  atomic.Int32
	generateUser        string // last user prompt passed to Generate
	cheapGenerateUser   string // last user prompt passed to GenerateCheap
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

func (m *mockGemini) EmbedWithOptions(ctx context.Context, text string, _ gemini.EmbedOptions) ([]float32, error) {
	return m.Embed(ctx, text)
}

func (m *mockGemini) Generate(_ context.Context, _, user string) (string, error) {
	m.generateUser = user
	if m.generateResult != nil {
		return m.generateResult()
	}
	return "", nil
}

func (m *mockGemini) GenerateCheap(_ context.Context, _, user string) (string, error) {
	m.cheapGenerateCalls.Add(1)
	m.cheapGenerateUser = user
	if m.cheapGenerateResult != nil {
		return m.cheapGenerateResult()
	}
	return m.Generate(context.Background(), "", user)
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

// onePixelPNGHex is a valid 1x1 PNG; http.DetectContentType sniffs it as image/png.
const onePixelPNGHex = "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000a49444154789c6260000000000200e221bc330000000049454e44ae426082"

// --- Part A (agent-mem-6b5): the real downloadWithAuth + :67 wrap retryability. ---

// serveStatus starts a test server that always responds with the given status.
func serveStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/file"
}

// runDownloadAndClassify drives the real handler far enough to hit the download
// wrap at describe_attachment.go:67, then returns the error it produced. The
// download fails before any DB access, so DB may be nil.
func runDownloadAndClassify(t *testing.T, url string) error {
	t.Helper()
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)
	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID:      "slack_file:FDL",
		ExternalURL: url,
		Mime:        "image/png",
		Source:      "slack",
	})
	return h.Handler(context.Background(), payload)
}

func TestDescribeAttachment_Download403NotRetryable(t *testing.T) {
	err := runDownloadAndClassify(t, serveStatus(t, http.StatusForbidden))
	if err == nil {
		t.Fatal("expected an error for a 403 download")
	}
	if jobs.IsRetryable(err) {
		t.Errorf("403 through the describe_attachment download wrap must NOT be retryable; got retryable err: %v", err)
	}
}

func TestDescribeAttachment_Download503Retryable(t *testing.T) {
	err := runDownloadAndClassify(t, serveStatus(t, http.StatusServiceUnavailable))
	if err == nil {
		t.Fatal("expected an error for a 503 download")
	}
	if !jobs.IsRetryable(err) {
		t.Errorf("503 through the describe_attachment download wrap must be retryable; got: %v", err)
	}
}

func TestDescribeAttachment_DownloadNetworkErrorRetryable(t *testing.T) {
	// A server that is immediately closed yields connection-refused, i.e. a plain
	// network error with no HTTP status and no ErrFatal — must stay retryable.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/file"
	srv.Close()
	err := runDownloadAndClassify(t, url)
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	if !jobs.IsRetryable(err) {
		t.Errorf("a plain network error through the download wrap must be retryable; got: %v", err)
	}
}

// --- Part B1 (agent-mem-16e): byte sniff before the vision call. ---

func TestSniffImageBytes(t *testing.T) {
	png, err := hex.DecodeString(onePixelPNGHex)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	cases := []struct {
		name    string
		data    []byte
		mime    string
		wantErr bool
	}{
		{"empty download", nil, "image/png", true},
		{"auth html served as image", []byte("<!DOCTYPE html><html><body>Please sign in</body></html>"), "image/png", true},
		{"plain text served as image", []byte("please log in to continue to your workspace account today"), "image/png", true},
		{"real png", png, "image/png", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := sniffImageBytes(c.data, c.mime)
			if (err != nil) != c.wantErr {
				t.Errorf("sniffImageBytes(%s): err=%v, wantErr=%v", c.name, err, c.wantErr)
			}
		})
	}
}

// --- Part B2 (agent-mem-16e): non-result detection. ---

func TestIsNonResult(t *testing.T) {
	// A long, legitimate description of a screenshot OF an error dialog whose text
	// literally quotes the failure markers. This is the critical guard: it must be
	// false, and it would flip to true if the length condition were dropped.
	longRealDialog := "This is a detailed description of a screenshot showing an error dialog on the Wego admin console. " +
		"The centered dialog box displays the message: \"Image processing failed - unable to process the attachment.\" " +
		"A red warning triangle sits to the left of the text. Below the message are two buttons labelled Retry and Cancel, " +
		"and a collapsible Details section listing the request id, the timestamp, and the upload size. " +
		strings.Repeat("The surrounding page shows the standard left navigation and a data table of prior uploads. ", 8)

	cases := []struct {
		name        string
		description string
		ocr         string
		want        bool
	}{
		{
			name:        "observed production payload",
			description: "Image processing failed - unable to process the attachment due to technical limitations",
			ocr:         "No text could be extracted",
			want:        true,
		},
		{name: "both empty after trim", description: "   ", ocr: "\n\t ", want: true},
		{name: "long real description quoting the markers", description: longRealDialog, ocr: "Retry Cancel Details", want: false},
		{name: "normal description and ocr", description: "A photo of a whiteboard with the sprint plan sketched out.", ocr: "Sprint 42 goals", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNonResult(c.description, c.ocr); got != c.want {
				t.Errorf("isNonResult() = %v, want %v (len(desc)=%d)", got, c.want, len(strings.TrimSpace(c.description)))
			}
		})
	}
}

func TestImageMimeFromURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://files.slack.com/files-pri/T1/F1/shot.png", "image/png"},
		{"https://files.slack.com/files-pri/T1/F1/photo.JPG?t=abc", "image/jpeg"},
		{"https://files.slack.com/files-pri/T1/F1/anim.gif", "image/gif"},
		{"https://files.slack.com/files-pri/T1/F1/noext", "image/png"},
	}
	for _, c := range cases {
		if got := imageMimeFromURL(c.url); got != c.want {
			t.Errorf("imageMimeFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// --- Criterion 4 (agent-mem-16e): a non-result is never written. Scratch DB only. ---

func TestDescribeAttachment_NonResultNotPersisted(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	nodeID := "slack_file:F0BP8ULST4L"
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph.nodes (id, type, natural_key, url, machine_id) VALUES ($1,'slack_file',$2,$3,'test')`,
		nodeID, "F0BP8ULST4L", "https://example.test/img.png"); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	// Serve real PNG bytes so the B1 sniff passes and the flow reaches the B2 gate.
	png, err := hex.DecodeString(onePixelPNGHex)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	url := serveBytes(t, png, "image/png")

	gem := &mockGemini{
		// The exact observed production non-result.
		describeResult: func() (string, string, []string, error) {
			return "Image processing failed - unable to process the attachment due to technical limitations",
				"No text could be extracted", nil, nil
		},
		embedResult: func() ([]float32, error) { return []float32{0.1}, nil },
	}
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Gemini: gem}
	h := NewDescribeAttachmentHandler(deps)
	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID: nodeID, ExternalURL: url, Mime: "image/png", Source: "slack",
	})

	err = h.Handler(ctx, payload)
	if err == nil {
		t.Fatal("expected a non-result to return an error")
	}
	if !jobs.IsRetryable(err) {
		t.Errorf("non-result error should be retryable so it fails out at MaxAttempts; got: %v", err)
	}

	var bodies int
	if e := pool.QueryRow(ctx, `SELECT count(*) FROM graph.artifact_bodies WHERE node_id=$1`, nodeID).Scan(&bodies); e != nil {
		t.Fatalf("count artifact_bodies: %v", e)
	}
	if bodies != 0 {
		t.Errorf("expected NO artifact_bodies row for a non-result; found %d", bodies)
	}

	var idx int
	if e := pool.QueryRow(ctx, `SELECT count(*) FROM graph.artifact_index WHERE node_id=$1`, nodeID).Scan(&idx); e != nil {
		t.Fatalf("count artifact_index: %v", e)
	}
	if idx != 0 {
		t.Errorf("expected NO artifact_index row for a non-result; found %d", idx)
	}

	var body *string
	if e := pool.QueryRow(ctx, `SELECT body FROM graph.nodes WHERE id=$1`, nodeID).Scan(&body); e != nil {
		t.Fatalf("select node body: %v", e)
	}
	if body != nil {
		t.Errorf("expected node body to remain unset for a non-result; got %q", *body)
	}
	if gem.describeCalls.Load() != 1 {
		t.Errorf("expected Gemini.Describe called once; got %d", gem.describeCalls.Load())
	}
}

// --- Part B3 (agent-mem-16e): bounded, dedup'd backfill. Scratch DB only. ---

func seedAttachmentBody(t *testing.T, pool *pgxpool.Pool, nodeID, url, body string) {
	t.Helper()
	ctx := context.Background()
	nk := nodeID
	if i := strings.IndexByte(nodeID, ':'); i >= 0 {
		nk = nodeID[i+1:]
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph.nodes (id, type, natural_key, url, machine_id) VALUES ($1,'slack_file',$2,$3,'test') ON CONFLICT (id) DO NOTHING`,
		nodeID, nk, url); err != nil {
		t.Fatalf("seed node %s: %v", nodeID, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at, machine_id) VALUES ($1,$2,NOW(),'test')
		 ON CONFLICT (node_id) DO UPDATE SET body_full=EXCLUDED.body_full`,
		nodeID, body); err != nil {
		t.Fatalf("seed artifact_bodies %s: %v", nodeID, err)
	}
}

func TestBackfillFailedAttachments(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	poison := "slack_file:FPOISON"
	seedAttachmentBody(t, pool, poison, "https://example.test/bad.png",
		"Image processing failed - unable to process the attachment due to technical limitations\n\nOCR:\nNo text could be extracted")

	// A legitimate long description that merely mentions "failed" mid-text — must
	// NOT be matched by the backfill (the SQL markers are anchored at the start).
	legit := "slack_file:FLEGIT"
	longBody := "Screenshot of the payments dashboard: a red banner reads that a transaction failed. " +
		strings.Repeat("The table below lists prior attempts with timestamps and amounts. ", 6)
	seedAttachmentBody(t, pool, legit, "https://example.test/good.png", longBody)

	matched, enqueued := BackfillFailedAttachments(ctx, pool, zerolog.Nop(), 50)
	if matched != 1 || enqueued != 1 {
		t.Fatalf("first run: matched=%d enqueued=%d, want 1/1", matched, enqueued)
	}

	var poisonJobs int
	pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='describe_attachment' AND status='queued' AND payload->>'node_id'=$1`, poison).Scan(&poisonJobs)
	if poisonJobs != 1 {
		t.Errorf("expected 1 queued describe_attachment for %s; got %d", poison, poisonJobs)
	}
	var legitJobs int
	pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE payload->>'node_id'=$1`, legit).Scan(&legitJobs)
	if legitJobs != 0 {
		t.Errorf("legit long body must not be re-enqueued; got %d jobs", legitJobs)
	}

	// Second run: the node already has a queued job → dedup, matched again but enqueued 0.
	matched2, enqueued2 := BackfillFailedAttachments(ctx, pool, zerolog.Nop(), 50)
	if matched2 != 1 || enqueued2 != 0 {
		t.Fatalf("second run: matched=%d enqueued=%d, want 1/0 (dedup)", matched2, enqueued2)
	}
}

func TestBackfillFailedAttachments_RespectsCap(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	for i := range 3 {
		id := fmt.Sprintf("slack_file:FCAP%d", i)
		seedAttachmentBody(t, pool, id, "https://example.test/x.png",
			"Image processing failed - unable to process the attachment")
	}

	matched, enqueued := BackfillFailedAttachments(ctx, pool, zerolog.Nop(), 2)
	if matched != 2 || enqueued != 2 {
		t.Fatalf("cap: matched=%d enqueued=%d, want 2/2 (hard cap of 2)", matched, enqueued)
	}
}
