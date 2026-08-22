package main

import "testing"

func TestExtractURLs(t *testing.T) {
	raw := "https://youtu.be/abc123 http://example.com/g/1\nhttps://youtu.be/abc123 not-a-url magnet:?xt=urn:btih:deadbeef"
	got := extractURLs(raw)
	want := []string{"https://youtu.be/abc123", "http://example.com/g/1"}
	if len(got) != len(want) {
		t.Fatalf("got %d urls %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractURLsStripsWrappers(t *testing.T) {
	got := extractURLs("<https://example.com/watch?v=xyz>")
	if len(got) != 1 || got[0] != "https://example.com/watch?v=xyz" {
		t.Fatalf("got %v, want [https://example.com/watch?v=xyz]", got)
	}
}

func TestMediaName(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ": "youtube.com/watch",
		"https://youtu.be/dQw4w9WgXcQ":                "youtu.be/dQw4w9WgXcQ",
		"https://example.com":                         "example.com",
		"https://example.com/a/b/c/d":                 "example.com/c/d",
	}
	for in, want := range cases {
		if got := mediaName(in); got != want {
			t.Errorf("mediaName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadYtdlpProgress(t *testing.T) {
	log := "[youtube] Extracting URL\n" +
		"[download] Destination: video.mp4\n" +
		"[download]   1.2% of  123.45MiB at    1.23MiB/s ETA 00:42\n" +
		"[download]  45.2% of  123.45MiB at    2.00MiB/s ETA 00:30\n"
	got := readYtdlpProgress(log)
	want := "45.2% of 123.45MiB at 2.00MiB/s ETA 00:30"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadYtdlpProgressPostProcessing(t *testing.T) {
	log := "[download] 100% of 123.45MiB\n[Merger] Merging formats into \"video.mp4\"\n"
	if got := readYtdlpProgress(log); got != "post-processing" {
		t.Errorf("got %q, want post-processing", got)
	}
}

func TestReadYtdlpProgressNoMatch(t *testing.T) {
	if got := readYtdlpProgress("[youtube] Extracting URL\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReadGalleryDLProgress(t *testing.T) {
	log := "/downloads/site/user/1.jpg\n/downloads/site/user/2.jpg\n/downloads/site/user/3.jpg\n"
	got := readGalleryDLProgress(log)
	want := "3 file(s) — 3.jpg"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadGalleryDLProgressEmpty(t *testing.T) {
	if got := readGalleryDLProgress("gallery-dl: starting\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
