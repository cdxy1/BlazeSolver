# BlazeSolver

A compact Go proxy that controls undetected Chromium and returns page HTML and
cookies through an HTTP API. One package, six runtime files, no external Go
dependencies. Runtime, tests and release tooling do not require Python.

## Run

Install Go 1.24+ and Chrome/Chromium:

```sh
go build -o bin/blazesolver .
./bin/blazesolver
```

The default port is 8191; set `PORT` to change it. A matching patched ChromeDriver
is downloaded on first startup if no local driver is available.

Build and run the multistage image locally:

```sh
docker compose up --build -d
```

Or use the image published by this repository's CI:

```sh
docker run --rm -p 127.0.0.1:8191:8191 \
  --tmpfs /tmp:size=512M,mode=1777 \
  ghcr.io/cdxy1/blazesolver:latest
```

```sh
curl http://localhost:8191/v1 -H 'Content-Type: application/json' \
  -d '{"cmd":"request.get","url":"https://example.com/","maxTimeout":60000}'
```

Commands: `request.get`, `request.post`, `sessions.create`, `sessions.list`,
`sessions.destroy`. GET `/` reports readiness; GET `/health` is the health check.
See [API parameters and configuration](docs/API.md).

Explicit sessions reuse the browser and its cookies. Destroy sessions when done.
Requests without a session use fresh profiles. Requests for one session execute
sequentially; independent sessions can run concurrently.

## Code

| File | Responsibility |
| --- | --- |
| main.go | Startup banner, browser check, servers, shutdown |
| config.go | Configuration and input conversions |
| server.go | HTTP routes, validation, session lifecycle |
| browser.go | Driver patch, browser launch, WebDriver transport |
| solve.go | Navigation, cookies, challenges, results |
| metrics.go | Prometheus counters and histogram |

`LOG_LEVEL=debug` adds detail; `LOG_FILE` writes a log file. `NO_COLOR=1` disables
banner colors. Optional `CHROME_PATH`, `DRIVER_PATH` and `DRIVER_CACHE_DIR` select
local executables and the cache. Docker caches executable drivers under `/config`
and keeps temporary browser profiles in `/tmp`.

## Tests and CI

```sh
go test -race ./...
go vet ./...
go test -tags=integration -run TestRealUndetectedBrowser -v -timeout 180s .
```

The browser suite checks the actual undetected driver, HTTP responses, cookies,
screenshots, POST, local challenge/Turnstile interactions, timeout, isolation and
TTL. All pages are local; CI never sends requests to live real-estate websites.

CI runs on pushes and pull requests. It checks formatting, race tests, vet,
cross-compilation, real Chromium on native AMD64/ARM64 runners, and service startup
with a noexec tmpfs. The runtime image is checked for absence of Go and Python.

After checks pass, default-branch pushes publish `ghcr.io/cdxy1/blazesolver:latest`
and a `sha-<commit>` tag. Version tags such as `v3.5.3` also publish `3.5.3`,
`v3.5.3` and `3.5`; stable versions receive `latest`. Images support Linux AMD64,
ARM64, ARMv7 and 386. Pull requests cannot publish. Publishing uses the built-in
`GITHUB_TOKEN`; no personal access token is needed.

The first GHCR package may be private. Set its package visibility to Public after
publication if anonymous pulls are desired.

The Dockerfile has three stages: Go compilation, preparation of browser dependency
packages, and the final Debian runtime. Only the compiled binary, license and
browser dependencies are copied to the final stage; there is no Go compiler.

Version tags additionally create Linux/Windows release archives with bundled
Chrome. Update package.json before tagging. To build an archive manually on Linux,
install curl, jq, unzip, zip and tar, then run `bash tools/build-release.sh linux`
or `bash tools/build-release.sh windows`.

## License

MIT. Go implementation copyright 2026 cdxy1. Existing copyright notices are
retained in [LICENSE](LICENSE).
