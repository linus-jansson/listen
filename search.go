package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/client"
)

const (
	searchPageSize   = 5
	searchSessionTTL = 15 * time.Minute
	tpbAPIBase       = "https://apibay.org"
)

type tpbResult struct {
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	Seeders  string `json:"seeders"`
	Leechers string `json:"leechers"`
	Size     string `json:"size"`
}

type searchSession struct {
	Results     []tpbResult
	Page        int
	Query       string
	Created     time.Time
	OwnerUserID string
}

var (
	searchSessions   = make(map[string]*searchSession)
	searchSessionsMu sync.Mutex
)

func newSessionID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func pruneSearchSessions() {
	cutoff := time.Now().Add(-searchSessionTTL)
	searchSessionsMu.Lock()
	defer searchSessionsMu.Unlock()
	for id, sess := range searchSessions {
		if sess.Created.Before(cutoff) {
			delete(searchSessions, id)
		}
	}
}

func searchTorrents(query string) ([]tpbResult, error) {
	u := tpbAPIBase + "/q.php?q=" + url.QueryEscape(query) + "&cat=0"
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	var results []tpbResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	// TPB returns a single sentinel entry when there are no results
	if len(results) == 1 && results[0].InfoHash == "0000000000000000000000000000000000000000" {
		return nil, nil
	}
	return results, nil
}

func buildMagnetFromResult(r tpbResult, trackers string) string {
	var sb strings.Builder
	sb.WriteString("magnet:?xt=urn:btih:")
	sb.WriteString(strings.ToLower(r.InfoHash))
	sb.WriteString("&dn=")
	sb.WriteString(url.QueryEscape(r.Name))
	for _, tr := range strings.Split(trackers, ",") {
		tr = strings.TrimSpace(tr)
		if tr != "" {
			sb.WriteString("&tr=")
			sb.WriteString(url.QueryEscape(tr))
		}
	}
	return sb.String()
}

func formatBytes(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "?"
	}
	switch {
	case n >= 1 << 30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1 << 20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1 << 10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func buildSearchEmbed(results []tpbResult, page int, query string) *discordgo.MessageEmbed {
	total := len(results)
	totalPages := (total + searchPageSize - 1) / searchPageSize

	start := page * searchPageSize
	end := start + searchPageSize
	if end > total {
		end = total
	}

	var desc strings.Builder
	for i, r := range results[start:end] {
		seeds, _ := strconv.Atoi(r.Seeders)
		leeches, _ := strconv.Atoi(r.Leechers)
		fmt.Fprintf(&desc, "**%d. %s**\n▲ %d seeds  ▼ %d leeches  📦 %s\n\n",
			i+1, truncate(r.Name, 80), seeds, leeches, formatBytes(r.Size))
	}

	return &discordgo.MessageEmbed{
		Title:       "🔍 " + query,
		Description: strings.TrimRight(desc.String(), "\n"),
		Color:       0x5865F2,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Page %d/%d · %d results · tpb", page+1, totalPages, total),
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

func buildSearchComponents(results []tpbResult, page int, sessionID string) []discordgo.MessageComponent {
	total := len(results)
	totalPages := (total + searchPageSize - 1) / searchPageSize

	start := page * searchPageSize
	end := start + searchPageSize
	if end > total {
		end = total
	}

	var options []discordgo.SelectMenuOption
	for i, r := range results[start:end] {
		seeds, _ := strconv.Atoi(r.Seeders)
		label := truncate(fmt.Sprintf("%d. %s", i+1, r.Name), 100)
		desc := truncate(fmt.Sprintf("▲ %d seeds · %s", seeds, formatBytes(r.Size)), 100)
		options = append(options, discordgo.SelectMenuOption{
			Label:       label,
			Value:       strconv.Itoa(i),
			Description: desc,
		})
	}

	rows := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "srch_dl:" + sessionID,
					Placeholder: "Select a result to download…",
					Options:     options,
				},
			},
		},
	}

	if totalPages > 1 {
		rows = append(rows, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "◀ Prev",
					Style:    discordgo.SecondaryButton,
					CustomID: "srch_prev:" + sessionID,
					Disabled: page == 0,
				},
				discordgo.Button{
					Label:    "Next ▶",
					Style:    discordgo.SecondaryButton,
					CustomID: "srch_next:" + sessionID,
					Disabled: page >= totalPages-1,
				},
			},
		})
	}

	return rows
}

func handleSearch(s *discordgo.Session, i *discordgo.InteractionCreate, cfg *config, query string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("defer response", "err", err)
		return
	}

	editContent := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			slog.Error("edit response", "err", err)
		}
	}

	results, err := searchTorrents(query)
	if err != nil {
		slog.Error("search torrents", "query", query, "err", err)
		editContent(fmt.Sprintf("Search failed: %v", err))
		return
	}
	if len(results) == 0 {
		editContent(fmt.Sprintf("No results for **%s**.", query))
		return
	}

	go pruneSearchSessions()

	sessionID := newSessionID()
	sess := &searchSession{Results: results, Page: 0, Query: query, Created: time.Now(), OwnerUserID: interactionUserID(i)}
	searchSessionsMu.Lock()
	searchSessions[sessionID] = sess
	searchSessionsMu.Unlock()

	embed := buildSearchEmbed(results, 0, query)
	components := buildSearchComponents(results, 0, sessionID)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &components,
	}); err != nil {
		slog.Error("edit search response", "err", err)
	}
}

func handleSearchNav(s *discordgo.Session, i *discordgo.InteractionCreate, sessionID string, delta int) {
	searchSessionsMu.Lock()
	sess, ok := searchSessions[sessionID]
	var results []tpbResult
	var page int
	var query string
	if ok {
		// Check TTL expiry
		if time.Since(sess.Created) > searchSessionTTL {
			delete(searchSessions, sessionID)
			ok = false
		} else if sess.OwnerUserID != interactionUserID(i) {
			// Check ownership
			searchSessionsMu.Unlock()
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "You don't have permission to use this search session.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}
	}
	if ok {
		totalPages := (len(sess.Results) + searchPageSize - 1) / searchPageSize
		newPage := sess.Page + delta
		if newPage < 0 {
			newPage = 0
		} else if newPage >= totalPages {
			newPage = totalPages - 1
		}
		sess.Page = newPage
		results = sess.Results
		page = sess.Page
		query = sess.Query
	}
	searchSessionsMu.Unlock()

	if !ok {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Search session expired. Run `/search` again.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	embed := buildSearchEmbed(results, page, query)
	components := buildSearchComponents(results, page, sessionID)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
		},
	})
}

func handleSearchDownload(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client, cfg *config, sessionID string, values []string) {
	if len(values) == 0 {
		return
	}
	localIdx, err := strconv.Atoi(values[0])
	if err != nil {
		return
	}

	searchSessionsMu.Lock()
	sess, ok := searchSessions[sessionID]
	var result tpbResult
	if ok {
		// Check TTL expiry
		if time.Since(sess.Created) > searchSessionTTL {
			delete(searchSessions, sessionID)
			ok = false
		} else if sess.OwnerUserID != interactionUserID(i) {
			// Check ownership
			searchSessionsMu.Unlock()
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "You don't have permission to use this search session.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		} else if localIdx < 0 {
			// Reject negative indices
			ok = false
		} else {
			globalIdx := sess.Page*searchPageSize + localIdx
			if globalIdx < 0 || globalIdx >= len(sess.Results) {
				ok = false
			} else {
				result = sess.Results[globalIdx]
			}
		}
	}
	searchSessionsMu.Unlock()

	if !ok {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Search session expired. Run `/search` again.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	magnet := buildMagnetFromResult(result, cfg.btTrackers)
	name := result.Name
	channelID := i.ChannelID

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("⏬ Downloading **%s**…", truncate(name, 100)),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()

		start := time.Now()
		exitCode, logs, err := runDownloader(ctx, docker, cfg, magnet)

		var msg string
		switch {
		case err != nil:
			slog.Error("search download error", "name", name, "err", err)
			msg = fmt.Sprintf("❌ **%s** — download failed: %v", truncate(name, 80), err)
		case exitCode == 0:
			dur := time.Since(start).Round(time.Second).String()
			slog.Info("search download complete", "name", name, "duration", dur)
			msg = fmt.Sprintf("✅ **%s** — complete in %s", truncate(name, 80), dur)
		default:
			slog.Warn("search download failed", "name", name, "exit_code", exitCode)
			msg = fmt.Sprintf("❌ **%s** — exit %d: %s", truncate(name, 80), exitCode, lastNonEmptyLine(logs))
		}

		if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Content:         msg,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		}); err != nil {
			slog.Error("send download notice", "err", err)
		}
	}()
}

func handleSearchComponent(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client, cfg *config) {
	data := i.MessageComponentData()
	parts := strings.SplitN(data.CustomID, ":", 2)
	if len(parts) != 2 {
		return
	}
	action, sessionID := parts[0], parts[1]
	switch action {
	case "srch_prev":
		handleSearchNav(s, i, sessionID, -1)
	case "srch_next":
		handleSearchNav(s, i, sessionID, +1)
	case "srch_dl":
		handleSearchDownload(s, i, docker, cfg, sessionID, data.Values)
	}
}
