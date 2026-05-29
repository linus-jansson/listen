FROM golang:1.25-bookworm

# Base packages for development and debugging inside the container.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        docker.io \
        git \
        make \
        openssh-client \
        procps \
        sudo \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspaces/listen
