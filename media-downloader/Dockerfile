FROM python:3.12-alpine
RUN apk add --no-cache ffmpeg ca-certificates \
    && pip install --no-cache-dir yt-dlp gallery-dl
WORKDIR /downloads
