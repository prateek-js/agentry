package mcp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakePNG is the smallest valid PNG (header + a single black pixel),
// base64-encoded. We don't decode it as an actual image anywhere — the
// extractor only verifies that base64 decodes successfully.
const fakePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII="

func TestExtractRenderableImagesNone(t *testing.T) {
	res := CodeExecResult{
		Status: "ok",
		Stdout: "hi\n",
		Result: map[string]any{"text/plain": "42"},
	}
	stripped, imgs := extractRenderableImages(res)
	if len(imgs) != 0 {
		t.Errorf("got %d images; want 0", len(imgs))
	}
	if stripped.Result["text/plain"] != "42" {
		t.Errorf("text/plain mangled: %+v", stripped.Result)
	}
}

func TestExtractRenderableImagesFromResult(t *testing.T) {
	res := CodeExecResult{
		Status: "ok",
		Result: map[string]any{
			"text/plain": "<Figure>",
			"image/png":  fakePNGBase64,
		},
	}
	stripped, imgs := extractRenderableImages(res)
	if len(imgs) != 1 {
		t.Fatalf("got %d images; want 1", len(imgs))
	}
	img, ok := imgs[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content[0] is %T; want *mcp.ImageContent", imgs[0])
	}
	if img.MIMEType != "image/png" {
		t.Errorf("mime = %q; want image/png", img.MIMEType)
	}
	wantBytes, _ := base64.StdEncoding.DecodeString(fakePNGBase64)
	if len(img.Data) != len(wantBytes) {
		t.Errorf("decoded len = %d; want %d", len(img.Data), len(wantBytes))
	}
	// Heavy bytes replaced with marker.
	marker, _ := stripped.Result["image/png"].(string)
	if !strings.Contains(marker, "rendered as image #1") {
		t.Errorf("marker = %q; want '… rendered as image #1 …'", marker)
	}
	// text/plain untouched.
	if stripped.Result["text/plain"] != "<Figure>" {
		t.Errorf("text/plain mangled: %+v", stripped.Result)
	}
}

func TestExtractRenderableImagesFromDisplays(t *testing.T) {
	res := CodeExecResult{
		Status: "ok",
		Displays: []map[string]any{
			{
				"data": map[string]any{
					"text/plain": "<chart1>",
					"image/png":  fakePNGBase64,
				},
			},
			{
				"data": map[string]any{
					"text/plain": "<chart2>",
					"image/jpeg": fakePNGBase64, // misnamed but exercises jpeg path
				},
			},
		},
	}
	_, imgs := extractRenderableImages(res)
	if len(imgs) != 2 {
		t.Fatalf("got %d images; want 2", len(imgs))
	}
	got := []string{}
	for _, c := range imgs {
		got = append(got, c.(*mcp.ImageContent).MIMEType)
	}
	if got[0] != "image/png" || got[1] != "image/jpeg" {
		t.Errorf("mimes = %v; want [image/png image/jpeg]", got)
	}
}

func TestExtractRenderableImagesPreservesSVG(t *testing.T) {
	// SVG is not a renderable-image MIME (text-only); should stay in JSON.
	svgBody := `<svg xmlns="http://www.w3.org/2000/svg"><circle r="5"/></svg>`
	res := CodeExecResult{
		Status: "ok",
		Result: map[string]any{
			"text/plain":    "<Figure>",
			"image/svg+xml": svgBody,
		},
	}
	stripped, imgs := extractRenderableImages(res)
	if len(imgs) != 0 {
		t.Fatalf("got %d images; want 0 (SVG should stay inline)", len(imgs))
	}
	if stripped.Result["image/svg+xml"] != svgBody {
		t.Errorf("SVG was stripped: %+v", stripped.Result)
	}
}

func TestExtractRenderableImagesSkipsMalformedBase64(t *testing.T) {
	res := CodeExecResult{
		Status: "ok",
		Displays: []map[string]any{
			{"data": map[string]any{"image/png": "not-base64-at-all"}},
		},
	}
	_, imgs := extractRenderableImages(res)
	if len(imgs) != 0 {
		t.Errorf("malformed base64 should be skipped, got %d images", len(imgs))
	}
}

func TestExtractRenderableImagesCapped(t *testing.T) {
	// 10 displays, each with one PNG; cap is 8.
	displays := make([]map[string]any, 10)
	for i := range displays {
		displays[i] = map[string]any{
			"data": map[string]any{"image/png": fakePNGBase64},
		}
	}
	res := CodeExecResult{Status: "ok", Displays: displays}

	stripped, imgs := extractRenderableImages(res)
	if len(imgs) != maxInlineImages {
		t.Errorf("got %d images; want cap=%d", len(imgs), maxInlineImages)
	}
	// Hint in stdout that some images were elided.
	if !strings.Contains(stripped.Stdout, "of 10 images rendered inline") {
		t.Errorf("expected cap-hint in stdout; got %q", stripped.Stdout)
	}
}
