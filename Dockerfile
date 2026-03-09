FROM ubuntu:24.04

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install base dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    gnupg \
    openssh-client \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js (LTS) for Claude Code
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Install GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    | gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Install GitLab CLI
RUN GLAB_VERSION=$(curl -fsSL https://gitlab.com/api/v4/projects/34675721/releases/permalink/latest | grep -o '"tag_name":"[^"]*"' | cut -d'"' -f4 | sed 's/^v//') \
    && curl -fsSL "https://gitlab.com/gitlab-org/cli/-/releases/v${GLAB_VERSION}/downloads/glab_${GLAB_VERSION}_linux_$(dpkg --print-architecture).deb" -o /tmp/glab.deb \
    && dpkg -i /tmp/glab.deb \
    && rm /tmp/glab.deb

# Copy pre-built toad binary (injected by GoReleaser via TARGETPLATFORM)
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/toad /usr/local/bin/toad

# Create toad user (uid/gid 1000 to match EFS access point)
RUN groupadd -f -g 1000 toad && useradd -u 1000 -g 1000 -m -s /bin/bash toad

USER toad
WORKDIR /home/toad

# Toad stores state in ~/.toad/ and auth in ~/.claude/, ~/.config/gh/, ~/.config/glab-cli/
# These paths should be on the EFS mount at /home/toad

ENTRYPOINT ["toad"]
