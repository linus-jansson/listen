package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	labelBot     = "listen.bot"
	labelName    = "listen.name"
	labelCreated = "listen.created"
	labelValue   = "magnet"
)

type config struct {
	token            string
	appID            string
	guildID          string
	downloadHostPath string
	downloaderImage  string
}

func loadConfig() (*config, error) {
	c := &config{
		token:            os.Getenv("DISCORD_TOKEN"),
		appID:            os.Getenv("DISCORD_APP_ID"),
		guildID:          os.Getenv("DISCORD_GUILD_ID"),
		downloadHostPath: os.Getenv("DOWNLOAD_HOST_PATH"),
		downloaderImage:  os.Getenv("DOWNLOADER_IMAGE"),
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
	return c, nil
}

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "magnet",
		Description: "Download a torrent from a magnet link into the shared folder",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "link",
				Description: "The magnet: URI",
				Required:    true,
			},
		},
	},
	{
		Name:        "downloads",
		Description: "List active and recent magnet downloads",
	},
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}
	defer docker.Close()

	dg, err := discordgo.New("Bot " + cfg.token)
	if err != nil {
		log.Fatalf("discord client: %v", err)
	}

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		switch data.Name {
		case "magnet":
			link := data.Options[0].StringValue()
			handleMagnet(s, i, docker, cfg, link)
		case "downloads":
			handleDownloads(s, i, docker)
		}
	})

	if err := dg.Open(); err != nil {
		log.Fatalf("open discord session: %v", err)
	}
	defer dg.Close()

	log.Println("registering slash commands...")
	registered, err := dg.ApplicationCommandBulkOverwrite(cfg.appID, cfg.guildID, slashCommands)
	if err != nil {
		log.Fatalf("register commands: %v", err)
	}
	scope := "globally"
	if cfg.guildID != "" {
		scope = "in guild " + cfg.guildID
	}
	log.Printf("registered %d command(s) %s", len(registered), scope)

	log.Println("bot is running. Ctrl+C to stop.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
}

func handleMagnet(s *discordgo.Session, i *discordgo.InteractionCreate, docker *client.Client, cfg *config, link string) {
	if !strings.HasPrefix(link, "magnet:?") {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "That doesn't look like a magnet link.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("defer response: %v", err)
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()

		edit := func(content string) {
			if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
				log.Printf("edit response: %v", err)
			}
		}

		edit("Starting download...")
		exitCode, logs, err := runDownloader(ctx, docker, cfg, link)
		if err != nil {
			edit(fmt.Sprintf("Download failed: %v", err))
			return
		}
		if exitCode == 0 {
			edit("Download complete.")
			return
		}
		edit(fmt.Sprintf("Download failed (exit %d):\n```\n%s\n```", exitCode, tailLines(logs, 20)))
	}()
}

func runDownloader(ctx context.Context, cli *client.Client, cfg *config, magnet string) (int64, string, error) {
	if _, err := cli.ImageInspect(ctx, cfg.downloaderImage); err != nil {
		pullCtx, pullCancel := context.WithTimeout(ctx, 2*time.Minute)
		if rc, perr := cli.ImagePull(pullCtx, cfg.downloaderImage, image.PullOptions{}); perr == nil {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		}
		pullCancel()
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: cfg.downloaderImage,
			Cmd: []string{
				"--seed-time=0",
				"--bt-stop-timeout=600",
				"--summary-interval=10",
				"--enable-color=false",
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
			Binds:      []string{cfg.downloadHostPath + ":/downloads"},
			AutoRemove: false,
		},
		nil, nil, "",
	)
	if err != nil {
		return 0, "", fmt.Errorf("create container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
		return 0, "", fmt.Errorf("start container: %w", err)
	}

	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	var exitCode int64
	select {
	case werr := <-errCh:
		if werr != nil {
			return 0, "", fmt.Errorf("wait container: %w", werr)
		}
	case s := <-statusCh:
		exitCode = s.StatusCode
	}

	logs := readLogs(cli, resp.ID, "all")
	return exitCode, logs, nil
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
		log.Printf("defer response: %v", err)
		return
	}

	edit := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			log.Printf("edit response: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelBot+"="+labelValue)),
	})
	if err != nil {
		edit(fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}
	if len(list) == 0 {
		edit("No downloads.")
		return
	}

	sort.Slice(list, func(a, b int) bool { return list[a].Created > list[b].Created })

	const maxRows = 20
	var b strings.Builder
	b.WriteString("```\n")
	for idx, c := range list {
		if idx >= maxRows {
			break
		}
		name := c.Labels[labelName]
		if name == "" {
			name = "magnet"
		}
		fmt.Fprintf(&b, "%s\n  %s\n", truncate(name, 70), formatStatus(ctx, docker, c.ID, c.State, c.Status))
	}
	b.WriteString("```")
	if len(list) > maxRows {
		fmt.Fprintf(&b, "\n_…and %d more_", len(list)-maxRows)
	}
	edit(b.String())
}

func formatStatus(ctx context.Context, cli *client.Client, id, state, statusStr string) string {
	if state == "running" {
		if p := readProgress(ctx, cli, id); p != "" {
			return "running — " + p
		}
		return "running (waiting for metadata)"
	}
	return statusStr
}

var progressRe = regexp.MustCompile(`\[#\w+\s+(\S+)/(\S+)\((\d+)%\)[^\]]*?DL:(\S+?)(?:\s+UL:\S+)?(?:\s+ETA:([^\]\s]+))?\]`)

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
		if m := progressRe.FindStringSubmatch(lines[j]); m != nil {
			eta := m[5]
			if eta == "" {
				eta = "?"
			}
			return fmt.Sprintf("%s%% (%s/%s) DL %s ETA %s", m[3], m[1], m[2], m[4], eta)
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
