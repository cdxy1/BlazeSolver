# Working on BlazeSolver

Keep the service in one Go package at the repository root. Prefer descriptive
names and direct control flow over frameworks and extra layers. Use the standard
library unless a dependency solves a substantial problem.

Preserve API commands, JSON field presence, defaults, cookie/POST behavior,
session lifecycle and undetected browser setup. Do not replace the patched driver
with ordinary WebDriver or change verification timings without browser tests.

Run gofmt, go test -race ./..., go vet ./... and local browser integration tests.
CI must use only local fixture pages, never live real-estate or protection sites.
The runtime and tests must not depend on Python or a copied reference service.
Report actual checks and unavailable platforms accurately. Parallel agents may
own distinct files after coordinating shared types.
