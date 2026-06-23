package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rs/zerolog"
)

// LiteParseConfig configures the subprocess invocation.
type LiteParseConfig struct {
	// BinPath is the path to the lit binary. Env: LITEPARSE_BIN_PATH, default "lit".
	BinPath string
	// ScreenshotEnabled controls whether page screenshots are generated for thin-text fallback.
	ScreenshotEnabled bool
	// TempDir is where input files and screenshots are written. Default: os.TempDir().
	TempDir string
}

// LiteParseResult is the normalised output used downstream.
type LiteParseResult struct {
	Pages         []LiteParsePage
	TotalTextLen  int
	Available     bool   // false if binary missing or invocation failed
	FailureReason string // populated when Available=false; non-fatal — caller falls back to Gemini
}

// LiteParsePage holds per-page text and optional screenshot bytes.
// Screenshot bytes are read inside ParseDocument before the temp dir is cleaned up,
// so callers never deal with file paths that may be deleted.
type LiteParsePage struct {
	PageNumber      int
	Text            string
	ScreenshotBytes []byte // nil if screenshots disabled or generation failed
}

// liteParseJSONPage mirrors the actual JSON output of `lit parse --format json`.
// Schema confirmed from crates/liteparse/src/output/json.rs:
//
//	{"pages": [{"page": 1, "width": ..., "height": ..., "text": "...", "text_items": [...]}]}
type liteParseJSONPage struct {
	Page   int     `json:"page"`
	Width  float32 `json:"width"`
	Height float32 `json:"height"`
	Text   string  `json:"text"`
}

type liteParseJSONResult struct {
	Pages []liteParseJSONPage `json:"pages"`
}

// mimeToExt maps MIME types to file extensions for the temp input file.
func mimeToExt(mime string) string {
	switch mime {
	case "application/pdf":
		return ".pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	default:
		return ".bin"
	}
}

// ParseDocument shells out to the `lit` CLI for the given bytes and MIME hint.
//
// Returns Available=false on any error so the caller can transparently fall
// back to Gemini multimodal — this is a non-fatal optimisation path.
//
// The actual CLI binary is `lit` (from run-llama/liteparse). Key flags:
//
//	lit parse <file> --format json --quiet
//	lit screenshot <file> -o <dir> --quiet
//
// Screenshot filenames written by lit: page_<N>.png (underscore, 1-indexed).
func ParseDocument(ctx context.Context, cfg LiteParseConfig, raw []byte, mime string, log zerolog.Logger) (LiteParseResult, error) {
	binPath := cfg.BinPath
	if binPath == "" {
		binPath = "lit"
	}

	// Resolve binary; treat missing binary as non-fatal.
	if _, err := exec.LookPath(binPath); err != nil {
		return LiteParseResult{
			Available:     false,
			FailureReason: fmt.Sprintf("lit binary not found at %q: %v", binPath, err),
		}, nil
	}

	tempBase := cfg.TempDir
	if tempBase == "" {
		tempBase = os.TempDir()
	}

	// Write raw bytes to a uniquely-named temp directory.
	tmpDir, err := os.MkdirTemp(tempBase, "liteparse-*")
	if err != nil {
		return LiteParseResult{
			Available:     false,
			FailureReason: fmt.Sprintf("create temp dir: %v", err),
		}, nil
	}
	// Always clean up on return — screenshot bytes are read into memory before we return.
	defer func() {
		if removeErr := os.RemoveAll(tmpDir); removeErr != nil {
			log.Warn().Err(removeErr).Str("tmpdir", tmpDir).Msg("liteparse: failed to remove temp dir")
		}
	}()

	inFile := filepath.Join(tmpDir, "input"+mimeToExt(mime))
	if err := os.WriteFile(inFile, raw, 0o600); err != nil {
		return LiteParseResult{
			Available:     false,
			FailureReason: fmt.Sprintf("write input file: %v", err),
		}, nil
	}

	// Run: lit parse <file> --format json --quiet
	//nolint:gosec // binPath comes from trusted config, not user input
	cmd := exec.CommandContext(ctx, binPath, "parse", inFile, "--format", "json", "--quiet")
	out, err := cmd.Output()
	if err != nil {
		reason := fmt.Sprintf("lit parse exited non-zero: %v", err)
		log.Warn().Str("reason", reason).Str("tmpdir", tmpDir).Msg("liteparse: parse failed, falling back to Gemini")
		return LiteParseResult{
			Available:     false,
			FailureReason: reason,
		}, nil
	}

	var parsed liteParseJSONResult
	if err := json.Unmarshal(out, &parsed); err != nil {
		reason := fmt.Sprintf("lit parse JSON decode: %v", err)
		log.Warn().Str("reason", reason).Msg("liteparse: invalid JSON output, falling back to Gemini")
		return LiteParseResult{
			Available:     false,
			FailureReason: reason,
		}, nil
	}

	// Optionally generate screenshots with a separate lit screenshot invocation.
	// lit names output files: page_<N>.png (underscore, 1-indexed).
	// We read the PNG bytes into memory here so the caller never touches temp files.
	screenshotBytes := map[int][]byte{}
	if cfg.ScreenshotEnabled && len(parsed.Pages) > 0 {
		screenshotDir := filepath.Join(tmpDir, "screenshots")
		if mkErr := os.MkdirAll(screenshotDir, 0o700); mkErr == nil {
			//nolint:gosec
			shotCmd := exec.CommandContext(ctx, binPath, "screenshot", inFile, "-o", screenshotDir, "--quiet")
			if shotErr := shotCmd.Run(); shotErr != nil {
				log.Warn().Err(shotErr).Msg("liteparse: screenshot generation failed, skipping screenshots")
			} else {
				for _, pg := range parsed.Pages {
					candidate := filepath.Join(screenshotDir, fmt.Sprintf("page_%d.png", pg.Page))
					if data, readErr := os.ReadFile(candidate); readErr == nil {
						screenshotBytes[pg.Page] = data
					}
				}
			}
		}
	}

	pages := make([]LiteParsePage, 0, len(parsed.Pages))
	totalLen := 0
	for _, pg := range parsed.Pages {
		totalLen += len(pg.Text)
		p := LiteParsePage{
			PageNumber:      pg.Page,
			Text:            pg.Text,
			ScreenshotBytes: screenshotBytes[pg.Page], // nil if not present
		}
		pages = append(pages, p)
	}

	return LiteParseResult{
		Pages:        pages,
		TotalTextLen: totalLen,
		Available:    true,
	}, nil
}

// combinePageTexts joins page texts with double newlines.
func combinePageTexts(pages []LiteParsePage) string {
	if len(pages) == 0 {
		return ""
	}
	result := pages[0].Text
	for _, p := range pages[1:] {
		result += "\n\n" + p.Text
	}
	return result
}
