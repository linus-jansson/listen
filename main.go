package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	labelBot     = "listen.bot"
	labelName    = "listen.name"
	labelCreated = "listen.created"
	labelValue   = "magnet"
)

const defaultTrackers = "udp://tracker.opentrackr.org:1337/announce," +
	"udp://open.tracker.cl:1337/announce," +
	"udp://open.demonii.com:1337/announce," +
	"udp://exodus.desync.com:6969/announce," +
	"udp://tracker.openbittorrent.com:6969/announce," +
	"udp://opentracker.i2p.rocks:6969/announce," +
	"udp://tracker.torrent.eu.org:451/announce," +
	"udp://open.stealth.si:80/announce," +
	"udp://tracker.tiny-vps.com:6969/announce," +
	"http://tracker.openbittorrent.com:80/announce"

type config struct {
	token            string
	appID            string
	guildID          string
	downloadHostPath string
	downloaderImage  string
	btStopTimeout    int
	btTrackers       string
}

func loadConfig() (*config, error) {
	c := &config{
		token:            os.Getenv("DISCORD_TOKEN"),
		appID:            os.Getenv("DISCORD_APP_ID"),
		guildID:          os.Getenv("DISCORD_GUILD_ID"),
		downloadHostPath: os.Getenv("DOWNLOAD_HOST_PATH"),
		downloaderImage:  os.Getenv("DOWNLOADER_IMAGE"),
		btStopTimeout:    600,
		btTrackers:       defaultTrackers,
	}
	if c.token == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN is required")
	}
	if c.appID == "" {
		return nil, fmt.Errorf("DISCORD_APP_ID is required")
	}
	if c.downloadHostPath == "" {
		return nil, fmt.Errorf("DOWNLOAD_HOST_PATH is required (host path mounted into spawned containers)")
	}
	if c.downloaderImage == "" {
		c.downloaderImage = "listen-downloader:latest"
	}
	if v := os.Getenv("BT_STOP_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.btStopTimeout = n
		}
	}
	if v := os.Getenv("BT_TRACKERS"); v != "" {
		c.btTrackers = v
	}
	return c, nil
}

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "magnet",
		Description: "Download one or more torrents from magnet links",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "links",
				Description: "One or more magnet: URIs, separated by spaces or newlines",
				Required:    true,
			},
		},
	},
	{
		Name:        "downloads",
		Description: "List active and recent magnet downloads",
	},
	{
		Name:        "clear",
		Description: "Remove finished (non-running) download containers",
	},
	{
		Name:        "restart",
		Description: "Restart all failed downloads (resumes from partial files)",
	},
	{
		Name:        "search",
		Description: "Search for torrents by name",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "query",
				Description: "What to search for",
				Required:    true,
			},
		},
	},
}

const downloadsRefreshButtonID = "downloads:refresh"

func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func interactionUser(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	if i.User != nil {
		return i.User.Username
	}
	return "unknown"
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func main() {
	setupLogging()

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error("docker client", "err", err)
		os.Exit(1)
	}
	defer docker.Close()

	if ping, err := docker.Ping(context.Background()); err != nil {
		slog.Warn("docker ping failed; check DOCKER_HOST / proxy", "err", err)
	} else {
		slog.Info("connected to docker", "api_version", ping.APIVersion)
	}

	dg, err := discordgo.New("Bot " + cfg.token)
	if err != nil {
		slog.Error("discord client", "err", err)
		os.Exit(1)
	}

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			data := i.ApplicationCommandData()
			slog.Info("command received",
				"command", data.Name,
				"user", interactionUser(i),
				"guild", i.GuildID,
			)
			switch data.Name {
			case "magnet":
				link := data.Options[0].StringValue()
				handleMagnet(s, i, docker, cfg, link)
			case "search":
				query := data.Options[0].StringValue()
				handleSearch(s, i, cfg, query)
			case "downloads":
				handleDownloads(s, i, docker)
			case "clear":
				handleClear(s, i, docker)
			case "restart":
				handleRestart(s, i, docker)
			}
		case discordgo.InteractionMessageComponent:
			data := i.MessageComponentData()
			slog.Info("component", "id", data.CustomID, "user", interactionUser(i))
			if strings.HasPrefix(data.CustomID, "srch_") {
				handleSearchComponent(s, i, docker, cfg)
			} else if data.CustomID == downloadsRefreshButtonID {
				handleDownloadsRefresh(s, i, docker)
			}
		}
	})

	if err := dg.Open(); err != nil {
		slog.Error("open discord session", "err", err)
		os.Exit(1)
	}
	defer dg.Close()

	registered, err := dg.ApplicationCommandBulkOverwrite(cfg.appID, cfg.guildID, slashCommands)
	if err != nil {
		slog.Error("register commands", "err", err)
		os.Exit(1)
	}
	scope := "global"
	if cfg.guildID != "" {
		scope = "guild:" + cfg.guildID
	}
	slog.Info("slash commands registered", "count", len(registered), "scope", scope)

	slog.Info("bot running; Ctrl+C to stop")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
}

func extractMagnets(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	seen := make(map[string]bool)
	var out []string
	for _, f := range fields {
		if strings.HasPrefix(f, "magnet:?") && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

type magnetJob struct {
	name   string
	status string
	done   bool
	ok     bool
}

func magnetEmbed(jobs []*magnetJob) *discordgo.MessageEmbed {
	done, failed := 0, 0
	var b strings.Builder
	for _, j := range jobs {
		icon := "⏳"
		if j.done {
			if j.ok {
				icon = "✅"
				done++
			} else {
				icon = "❌"
				failed++
			}
		}
		fmt.Fprintf(&b, "%s **%s**\n%s\n", icon, truncate(j.name, 80), j.status)
	}
	color := 0x5865F2
	if done+failed == len(jobs) {
		color = 0x57F287
		if failed > 0 {
			color = 0xED4245
		}
	}
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("Downloads — %d/%d complete", done, len(jobs)),
		Description: b.String(),
		Color:       color,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
}

func handleMagnet(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client, cfg *config, raw string) {
	magnets := extractMagnets(raw)
	if len(magnets) == 0 {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "No magnet links found. Paste one or more `magnet:?...` URIs separated by spaces or newlines.",
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

	jobs := make([]*magnetJob, len(magnets))
	for idx, m := range magnets {
		jobs[idx] = &magnetJob{name: magnetName(m), status: "queued"}
	}

	var mu sync.Mutex
	render := func() {
		mu.Lock()
		embed := magnetEmbed(jobs)
		mu.Unlock()
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{embed},
		}); err != nil {
			slog.Error("edit response", "err", err)
		}
	}
	set := func(idx int, status string, done, ok bool) {
		mu.Lock()
		jobs[idx].status = status
		jobs[idx].done = done
		jobs[idx].ok = ok
		mu.Unlock()
		render()
	}

	slog.Info("magnet command", "count", len(magnets))
	render()

	var wg sync.WaitGroup
	for idx, m := range magnets {
		wg.Add(1)
		go func(idx int, m string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
			defer cancel()

			name := jobs[idx].name
			set(idx, "starting…", false, false)
			start := time.Now()
			exitCode, logs, err := runDownloader(ctx, docker, cfg, m)
			switch {
			case err != nil:
				slog.Error("download error", "err", err, "name", name)
				set(idx, fmt.Sprintf("failed: %v", err), true, false)
			case exitCode == 0:
				dur := time.Since(start).Round(time.Second).String()
				slog.Info("download complete", "name", name, "duration", dur)
				set(idx, "complete in "+dur, true, true)
			default:
				slog.Warn("download exited non-zero", "name", name, "exit_code", exitCode)
				set(idx, fmt.Sprintf("failed (exit %d): %s", exitCode, lastNonEmptyLine(logs)), true, false)
			}
		}(idx, m)
	}
	wg.Wait()

	ok, failed := 0, 0
	var failedNames []string
	mu.Lock()
	for _, j := range jobs {
		if j.ok {
			ok++
		} else {
			failed++
			failedNames = append(failedNames, j.name)
		}
	}
	mu.Unlock()
	sendCompletionNotice(s, i, ok, failed, failedNames)
}

func sendCompletionNotice(s *discordgo.Session, i *discordgo.InteractionCreate, ok, failed int, failedNames []string) {
	uid := interactionUserID(i)
	var content string
	var allowed *discordgo.MessageAllowedMentions
	if failed > 0 {
		shown := failedNames
		extra := 0
		if len(shown) > 5 {
			extra = len(shown) - 5
			shown = shown[:5]
		}
		summary := fmt.Sprintf("❌ %d of %d download(s) failed: %s", failed, failed+ok, strings.Join(shown, ", "))
		if extra > 0 {
			summary += fmt.Sprintf(" (+%d more)", extra)
		}
		if uid != "" {
			content = "<@" + uid + "> " + summary
			allowed = &discordgo.MessageAllowedMentions{Users: []string{uid}}
		} else {
			content = summary
			allowed = &discordgo.MessageAllowedMentions{}
		}
	} else {
		content = fmt.Sprintf("✅ %d download(s) complete.", ok)
		allowed = &discordgo.MessageAllowedMentions{}
	}
	if _, err := s.ChannelMessageSendComplex(i.ChannelID, &discordgo.MessageSend{
		Content:         content,
		AllowedMentions: allowed,
	}); err != nil {
		slog.Error("completion notice", "err", err)
	}
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	for j := len(lines) - 1; j >= 0; j-- {
		if t := strings.TrimSpace(lines[j]); t != "" {
			return truncate(t, 200)
		}
	}
	return ""
}

func runDownloader(ctx context.Context, cli *client.Client, cfg *config, magnet string) (int64, string, error) {
	if _, err := cli.ImageInspect(ctx, cfg.downloaderImage); err != nil {
		slog.Debug("downloader image not present locally; attempting pull", "image", cfg.downloaderImage)
		pullCtx, pullCancel := context.WithTimeout(ctx, 2*time.Minute)
		if rc, perr := cli.ImagePull(pullCtx, cfg.downloaderImage, image.PullOptions{}); perr == nil {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		} else {
			slog.Debug("image pull failed (expected for locally-built images)", "image", cfg.downloaderImage, "err", perr)
		}
		pullCancel()
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: cfg.downloaderImage,
			Cmd: []string{
				"--seed-time=0",
				fmt.Sprintf("--bt-stop-timeout=%d", cfg.btStopTimeout),
				"--summary-interval=10",
				"--enable-color=false",
				"--enable-dht=true",
				"--enable-dht6=true",
				"--bt-enable-lpd=true",
				"--enable-peer-exchange=true",
				"--bt-tracker=" + cfg.btTrackers,
				"-d", "/downloads",
				magnet,
			},
			Tty: false,
			Labels: map[string]string{
				labelBot:     labelValue,
				labelName:    magnetName(magnet),
				labelCreated: time.Now().UTC().Format(time.RFC3339),
			},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeBind,
				Source: cfg.downloadHostPath,
				Target: "/downloads",
			}},
			AutoRemove: false,
		},
		nil, nil,
		fmt.Sprintf("listen-dl-%d", time.Now().UnixNano()),
	)
	if err != nil {
		return 0, "", fmt.Errorf("create container: %w", err)
	}
	slog.Info("container created", "id", resp.ID[:12], "image", cfg.downloaderImage)

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
		return 0, "", fmt.Errorf("start container: %w", err)
	}
	slog.Info("container started", "id", resp.ID[:12])

	exitCode, err := waitForExit(ctx, cli, resp.ID)
	if err != nil {
		return 0, "", fmt.Errorf("wait container: %w", err)
	}

	logs := readLogs(cli, resp.ID, "all")
	return exitCode, logs, nil
}

// waitForExit polls the container state until it stops, instead of holding a
// single long-lived ContainerWait connection — that connection sits idle for
// the whole download and gets reaped by the docker-socket-proxy's HAProxy
// idle timeout. Short inspect calls tolerate proxy hiccups and reconnects.
func waitForExit(ctx context.Context, cli *client.Client, id string) (int64, error) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	consecutiveErrs := 0
	for {
		info, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				return 0, fmt.Errorf("inspect: %w", err)
			}
			slog.Warn("container inspect failed; retrying", "id", id[:12], "attempt", consecutiveErrs, "err", err)
		} else {
			consecutiveErrs = 0
			if info.State != nil && !info.State.Running {
				return int64(info.State.ExitCode), nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func readLogs(cli *client.Client, id string, tail string) string {
	rc, err := cli.ContainerLogs(context.Background(), id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
	})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var out, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errBuf, rc); err != nil {
		return ""
	}
	if errBuf.Len() > 0 {
		out.WriteByte('\n')
		out.Write(errBuf.Bytes())
	}
	return out.String()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func handleDownloads(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("defer response", "err", err)
		return
	}
	editDownloadsResponse(s, i, docker)
}

func handleDownloadsRefresh(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		slog.Error("defer refresh response", "err", err)
		return
	}
	editDownloadsResponse(s, i, docker)
}

func editDownloadsResponse(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client) {
	embed, content, err := buildDownloadsEmbed(docker)
	components := []discordgo.MessageComponent{downloadsRefreshButtonRow()}
	if err != nil {
		msg := fmt.Sprintf("Failed to list downloads: %v", err)
		if _, editErr := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content:    &msg,
			Embeds:     &[]*discordgo.MessageEmbed{},
			Components: &components,
		}); editErr != nil {
			slog.Error("edit response", "err", editErr)
		}
		return
	}

	if content != "" {
		if _, editErr := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content:    &content,
			Embeds:     &[]*discordgo.MessageEmbed{},
			Components: &components,
		}); editErr != nil {
			slog.Error("edit response", "err", editErr)
		}
		return
	}

	empty := ""
	if _, editErr := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:    &empty,
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &components,
	}); editErr != nil {
		slog.Error("edit response", "err", editErr)
	}
}

func downloadsRefreshButtonRow() discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			CustomID: downloadsRefreshButtonID,
			Label:    "Refresh",
			Style:    discordgo.PrimaryButton,
		},
	}}
}

func buildDownloadsEmbed(docker *client.Client) (*discordgo.MessageEmbed, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelBot+"="+labelValue)),
	})
	if err != nil {
		slog.Error("list downloads", "err", err)
		return nil, "", err
	}
	slog.Info("downloads listed", "count", len(list))
	if len(list) == 0 {
		return nil, "No downloads yet.", nil
	}

	sort.Slice(list, func(a, b int) bool { return list[a].Created > list[b].Created })

	const maxRows = 25
	running, completed := 0, 0
	embed := &discordgo.MessageEmbed{
		Color:     0x5865F2,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	for idx, c := range list {
		if idx >= maxRows {
			break
		}
		name := c.Labels[labelName]
		if name == "" {
			name = "magnet"
		}
		status := formatStatus(ctx, docker, c.ID, c.State, c.Status)
		if c.State == "running" {
			running++
		} else if strings.HasPrefix(c.Status, "Exited (0)") {
			completed++
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  truncate(name, 256),
			Value: truncate(status, 1024),
		})
	}
	embed.Title = fmt.Sprintf("Downloads — %d running, %d done, %d total", running, completed, len(list))
	if len(list) > maxRows {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("…and %d more not shown", len(list)-maxRows),
		}
	}
	return embed, "", nil
}

func handleClear(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("defer response", "err", err)
		return
	}
	edit := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			slog.Error("edit response", "err", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	list, err := docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelBot+"="+labelValue)),
	})
	if err != nil {
		slog.Error("clear: list", "err", err)
		edit(fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}

	removed, skipped, failed := 0, 0, 0
	for _, c := range list {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			skipped++
			continue
		}
		if err := docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{}); err != nil {
			slog.Error("clear: remove", "id", c.ID[:12], "err", err)
			failed++
			continue
		}
		removed++
	}
	slog.Info("clear done", "removed", removed, "skipped_active", skipped, "failed", failed)

	msg := fmt.Sprintf("Removed **%d** finished container(s).", removed)
	if skipped > 0 {
		msg += fmt.Sprintf(" Left %d still running.", skipped)
	}
	if failed > 0 {
		msg += fmt.Sprintf(" %d could not be removed (see logs).", failed)
	}
	edit(msg)
}

var exitCodeRe = regexp.MustCompile(`Exited \((\d+)\)`)

func handleRestart(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("defer response", "err", err)
		return
	}
	edit := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			slog.Error("edit response", "err", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	list, err := docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelBot+"="+labelValue)),
	})
	if err != nil {
		slog.Error("restart: list", "err", err)
		edit(fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}

	channelID := i.ChannelID
	userID := interactionUserID(i)

	restarted, skipped := 0, 0
	var restartedNames []string
	var failures []string
	for _, c := range list {
		if !isFailed(c) {
			skipped++
			continue
		}
		name := c.Labels[labelName]
		if name == "" {
			name = c.ID[:12]
		}
		if err := docker.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
			slog.Error("restart: start", "id", c.ID[:12], "err", err)
			failures = append(failures, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		slog.Info("restarted", "id", c.ID[:12], "name", name)
		restarted++
		restartedNames = append(restartedNames, name)
		go watchAndNotify(s, docker, c.ID, name, channelID, userID)
	}

	if restarted == 0 && len(failures) == 0 {
		edit(fmt.Sprintf("No failed downloads to restart (%d not eligible).", skipped))
		return
	}

	msg := fmt.Sprintf("Restarted **%d** failed download(s).", restarted)
	if len(restartedNames) > 0 {
		shown := restartedNames
		extra := 0
		if len(shown) > 5 {
			extra = len(shown) - 5
			shown = shown[:5]
		}
		msg += "\n• " + strings.Join(shown, "\n• ")
		if extra > 0 {
			msg += fmt.Sprintf("\n…and %d more", extra)
		}
	}
	if len(failures) > 0 {
		msg += fmt.Sprintf("\n\n%d could not be started:\n• %s", len(failures), strings.Join(failures, "\n• "))
	}
	edit(msg)
}

func isFailed(c container.Summary) bool {
	switch c.State {
	case "running", "restarting", "paused", "removing":
		return false
	case "created", "dead":
		return true
	}
	if m := exitCodeRe.FindStringSubmatch(c.Status); m != nil {
		return m[1] != "0"
	}
	return false
}

func watchAndNotify(s *discordgo.Session, docker *client.Client, id, name, channelID, userID string) {
	exitCode, err := waitForExit(context.Background(), docker, id)
	if err != nil {
		slog.Error("restart watch", "err", err, "name", name)
		return
	}

	var content string
	var allowed *discordgo.MessageAllowedMentions
	if exitCode == 0 {
		content = fmt.Sprintf("✅ Restart complete: %s", name)
		allowed = &discordgo.MessageAllowedMentions{}
		slog.Info("restart complete", "name", name)
	} else if userID != "" {
		content = fmt.Sprintf("<@%s> ❌ Restart failed again (exit %d): %s", userID, exitCode, name)
		allowed = &discordgo.MessageAllowedMentions{Users: []string{userID}}
		slog.Warn("restart failed again", "name", name, "exit_code", exitCode)
	} else {
		content = fmt.Sprintf("❌ Restart failed again (exit %d): %s", exitCode, name)
		allowed = &discordgo.MessageAllowedMentions{}
	}
	if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:         content,
		AllowedMentions: allowed,
	}); err != nil {
		slog.Error("restart notice", "err", err)
	}
}

func formatStatus(ctx context.Context, cli *client.Client, id, state, statusStr string) string {
	if state == "running" {
		if p := readProgress(ctx, cli, id); p != "" {
			return "running — " + p
		}
		return "running (starting up)"
	}
	return statusStr
}

var (
	progressRe = regexp.MustCompile(`\[#\w+\s+(\S+)/(\S+)\((\d+)%\)[^\]]*?DL:(\S+?)(?:\s+UL:\S+)?(?:\s+ETA:([^\]\s]+))?\]`)
	metadataRe = regexp.MustCompile(`\[#\w+\s+0B/0B\s+CN:(\d+)\s+SD:\d+\s+DL:\S+\]`)
)

func readProgress(ctx context.Context, cli *client.Client, id string) string {
	logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rc, err := cli.ContainerLogs(logCtx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
	})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var out, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errBuf, rc); err != nil {
		return ""
	}
	text := out.String() + "\n" + errBuf.String()
	lines := strings.Split(text, "\n")
	for j := len(lines) - 1; j >= 0; j-- {
		line := lines[j]
		if m := progressRe.FindStringSubmatch(line); m != nil {
			eta := m[5]
			if eta == "" {
				eta = "?"
			}
			return fmt.Sprintf("%s%% (%s/%s) DL %s ETA %s", m[3], m[1], m[2], m[4], eta)
		}
		if m := metadataRe.FindStringSubmatch(line); m != nil {
			conns := m[1]
			suffix := ""
			if strings.Contains(text, "info hash is not authorized") {
				suffix = " (private tracker — auth required)"
			}
			return fmt.Sprintf("fetching metadata, %s peer(s)%s", conns, suffix)
		}
	}
	return ""
}

func magnetName(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "magnet"
	}
	q := u.Query()
	if dn := q.Get("dn"); dn != "" {
		return dn
	}
	if xt := q.Get("xt"); strings.HasPrefix(xt, "urn:btih:") {
		h := strings.TrimPrefix(xt, "urn:btih:")
		if len(h) > 12 {
			h = h[:12]
		}
		return "btih:" + h
	}
	return "magnet"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
