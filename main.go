package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
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
				"--summary-interval=0",
				"-d", "/downloads",
				magnet,
			},
			Tty: true,
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
	defer func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
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

	logs := readLogs(cli, resp.ID)
	return exitCode, logs, nil
}

func readLogs(cli *client.Client, id string) string {
	rc, err := cli.ContainerLogs(context.Background(), id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return ""
	}
	defer rc.Close()
	buf, _ := io.ReadAll(rc)
	return string(buf)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
