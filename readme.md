# CLIProxyAPI Plugins

Native plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Each plugin lives in its own directory with its configuration reference, build instructions, tests, and compatibility notes.

## Plugins

- [cpa-model-fallback-router](./cpa-model-fallback-router/README.md) — retries matching model requests through CPA with ordered fallback models, cooldowns, streaming-aware retry handling, and an optional execution transform for tool-using agent requests.
- [key-model-access-local](./key-model-access-local/README.md) — enforces per-key model allow and deny policies through CPA's request interceptor and provides a management API and settings UI.

See each plugin's README for installation, configuration, compatibility notes, and troubleshooting.
