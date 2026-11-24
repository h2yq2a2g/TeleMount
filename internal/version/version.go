package version

// Version is overridden at build time via -ldflags "-X telemount/internal/version.Version=v1.0.3".
var Version = "dev"

// GitHubRepo is the canonical upstream repository checked for auto-updates.
const GitHubRepo = "h2yq2a2g/TeleMount"
