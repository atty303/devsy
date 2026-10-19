FROM mcr.microsoft.com/devcontainers/base:ubuntu
USER root
RUN apt-get update && apt-get install -y --no-install-recommends python3 dbus gnome-keyring openssl ca-certificates && rm -rf /var/lib/apt/lists/*
USER vscode
