# listen

Simple discord bot to download, track and clear magnet links. Built for personal.

## Commands

| Command | What it does |
| --- | --- |
| `/magnet <links>` | Download torrents from magnet links (aria2c) |
| `/search <query>` | Search for torrents, browse results, download from a menu |
| `/video <urls>` | Download video/audio with yt-dlp |
| `/gallery <urls>` | Download image galleries with gallery-dl |
| `/downloads` | List active and recent downloads |
| `/clear` | Remove finished download containers |
| `/restart` | Restart failed downloads |

## Building the downloader images

Both downloader images are build-only services in `docker-compose.yml`:

```sh
docker compose --profile build-only build downloader media-downloader
docker compose up -d
```

`downloader` carries aria2c for magnet links; `media-downloader` carries yt-dlp,
gallery-dl and ffmpeg. Point `DOWNLOADER_IMAGE` / `MEDIA_DOWNLOADER_IMAGE` at
different images if you'd rather supply your own.
