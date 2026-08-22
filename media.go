package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/client"
)

// Tool names recorded on the listen.tool container label, so /downloads knows
// how to read each container's progress output.
const (
	toolAria2     = "aria2"
	toolYtdlp     = "yt-dlp"
	toolGalleryDL = "gallery-dl"
)

// extractURLs pulls http(s) URLs out of free-form user input, deduplicated and
// in the order given.
func extractURLs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	seen := make(map[string]bool)
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, "<>\"'")
		if !strings.HasPrefix(f, "http://") && !strings.HasPrefix(f, "https://") {
			continue
		}
		if _, err := url.Parse(f); err != nil || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// mediaName derives a short display name from a URL: the host plus the tail of
// the path, which is usually the video ID or gallery slug.
func mediaName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return truncate(raw, 80)
	}
	host := strings.TrimPrefix(u.Host, "www.")
	path := strings.Trim(u.Path, "/")
	if path == "" {
		if v := u.Query().Get("v"); v != "" {
			return host + "/" + v
		}
		return host
	}
	if parts := strings.Split(path, "/"); len(parts) > 2 {
		path = strings.Join(parts[len(parts)-2:], "/")
	}
	return truncate(host+"/"+path, 80)
}

func ytdlpSpec(cfg *config, link string) downloadSpec {
	return downloadSpec{
		image: cfg.mediaImage,
		cmd: []string{
			"yt-dlp",
			"--newline",
			"--no-color",
			"--no-playlist",
			"--restrict-filenames",
			"-P", "/downloads",
			"-o", "%(title)s [%(id)s].%(ext)s",
			link,
		},
		name: mediaName(link),
		tool: toolYtdlp,
	}
}

func galleryDLSpec(cfg *config, link string) downloadSpec {
	return downloadSpec{
		image: cfg.mediaImage,
		cmd: []string{
			"gallery-dl",
			"--no-colors",
			"-d", "/downloads",
			link,
		},
		name: mediaName(link),
		tool: toolGalleryDL,
	}
}

func handleMedia(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client, cfg *config, tool string, raw string) {
	links := extractURLs(raw)
	if len(links) == 0 {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "No URLs found. Paste one or more `http://` or `https://` links separated by spaces or newlines.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("defer response", "err", err)
		return
	}

	slog.Info("media command", "tool", tool, "count", len(links))
	specs := make([]downloadSpec, len(links))
	for idx, link := range links {
		if tool == toolYtdlp {
			specs[idx] = ytdlpSpec(cfg, link)
		} else {
			specs[idx] = galleryDLSpec(cfg, link)
		}
	}
	runJobsAndNotify(s, i, docker, cfg, specs)
}

var (
	// [download]  45.2% of  123.45MiB at    1.23MiB/s ETA 00:42
	ytdlpProgressRe = regexp.MustCompile(`\[download\]\s+(\d+\.?\d*)%\s+of\s+~?\s*(\S+)(?:\s+at\s+(\S+))?(?:\s+ETA\s+(\S+))?`)
	// [Merger] Merging formats into "..." / [ExtractAudio] Destination: ...
	ytdlpStageRe = regexp.MustCompile(`^\[(Merger|ExtractAudio|FixupM3u8|VideoConvertor)\]`)
)

// readYtdlpProgress finds the most recent yt-dlp progress line in the log tail.
func readYtdlpProgress(text string) string {
	lines := strings.Split(text, "\n")
	for j := len(lines) - 1; j >= 0; j-- {
		line := strings.TrimSpace(lines[j])
		if ytdlpStageRe.MatchString(line) {
			return "post-processing"
		}
		if m := ytdlpProgressRe.FindStringSubmatch(line); m != nil {
			out := fmt.Sprintf("%s%% of %s", m[1], m[2])
			if m[3] != "" {
				out += " at " + m[3]
			}
			if m[4] != "" {
				out += " ETA " + m[4]
			}
			return out
		}
	}
	return ""
}

// readGalleryDLProgress counts downloaded files; gallery-dl prints one
// destination path per line as it goes.
func readGalleryDLProgress(text string) string {
	files := 0
	var last string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/downloads/") {
			files++
			last = line
		}
	}
	if files == 0 {
		return ""
	}
	name := last
	if idx := strings.LastIndex(last, "/"); idx >= 0 {
		name = last[idx+1:]
	}
	return fmt.Sprintf("%d file(s) — %s", files, truncate(name, 60))
}
