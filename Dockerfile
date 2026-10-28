FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS go-builder
WORKDIR /src
COPY go.mod *.go package.json ./
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
RUN if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi \
    && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
       go build -trimpath -ldflags="-s -w" -o /out/blazesolver .

FROM debian:bookworm-slim AS browser-packages

# Build dummy packages to skip installing them and their dependencies
RUN apt-get update \
    && apt-get install -y --no-install-recommends equivs \
    && equivs-control libgl1-mesa-dri \
    && printf 'Section: misc\nPriority: optional\nStandards-Version: 3.9.2\nPackage: libgl1-mesa-dri\nVersion: 99.0.0\nDescription: Dummy package for libgl1-mesa-dri\n' >> libgl1-mesa-dri \
    && equivs-build libgl1-mesa-dri \
    && mv libgl1-mesa-dri_*.deb /libgl1-mesa-dri.deb \
    && equivs-control adwaita-icon-theme \
    && printf 'Section: misc\nPriority: optional\nStandards-Version: 3.9.2\nPackage: adwaita-icon-theme\nVersion: 99.0.0\nDescription: Dummy package for adwaita-icon-theme\n' >> adwaita-icon-theme \
    && equivs-build adwaita-icon-theme \
    && mv adwaita-icon-theme_*.deb /adwaita-icon-theme.deb

FROM debian:bookworm-slim AS runtime

# Copy dummy packages
COPY --from=browser-packages /*.deb /

# Install dependencies and create blazesolver user
# You can test Chromium running this command inside the container:
#    xvfb-run -s "-screen 0 1600x1200x24" chromium --no-sandbox
# The error traces is like this: "*** stack smashing detected ***: terminated"
# To check the package versions available you can use this command:
#    apt-cache madison chromium
WORKDIR /app
    # Install dummy packages
RUN dpkg -i /libgl1-mesa-dri.deb \
    && dpkg -i /adwaita-icon-theme.deb \
    # Install dependencies
    && apt-get update \
    && apt-get install -y --no-install-recommends chromium chromium-common chromium-driver xvfb dumb-init \
        procps curl vim xauth ca-certificates tzdata \
    # Remove temporary files and hardware decoding libraries
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /usr/lib/x86_64-linux-gnu/libmfxhw* \
    && rm -f /usr/lib/x86_64-linux-gnu/mfx/* \
    # Create blazesolver user
    && useradd --home-dir /app --shell /bin/sh blazesolver \
    && mv /usr/bin/chromedriver chromedriver \
    && chown -R blazesolver:blazesolver . \
    # Create config dir
    && mkdir /config \
    && chown blazesolver:blazesolver /config

VOLUME /config

COPY --from=go-builder --chown=blazesolver:blazesolver /out/blazesolver /app/blazesolver

COPY --chown=blazesolver:blazesolver LICENSE /app/LICENSE

LABEL org.opencontainers.image.title="BlazeSolver" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/cdxy1/BlazeSolver"

USER blazesolver
RUN mkdir -p "/app/.config/chromium/Crash Reports/pending"

EXPOSE 8191
EXPOSE 8192

# dumb-init avoids zombie chromium processes
ENTRYPOINT ["/usr/bin/dumb-init", "--"]

CMD ["/app/blazesolver"]

# Local build:
# docker build -t blazesolver .
# docker run --rm -p 8191:8191 --tmpfs /tmp:size=512M,mode=1777 blazesolver
# Multiarch build:
# docker buildx build --platform linux/386,linux/amd64,linux/arm/v7,linux/arm64/v8 .
