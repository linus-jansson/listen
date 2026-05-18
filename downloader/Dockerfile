FROM alpine:latest
RUN apk add --no-cache aria2 ca-certificates
WORKDIR /downloads
ENTRYPOINT ["aria2c"]
