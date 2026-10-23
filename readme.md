# CLIProxyAPI Plugins

Plugins and deployment tools for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Each package lives in its own directory with its configuration, setup instructions, and compatibility notes.

## Plugins

- [cpa-model-fallback-router](./cpa-model-fallback-router/README.md) — retries matching model requests through CPA with ordered fallback models, cooldowns, streaming-aware retry handling, and an optional execution transform for tool-using agent requests.
- [key-model-access-local](./key-model-access-local/README.md) — enforces per-key model allow and deny policies through CPA's request interceptor and provides a management API and settings UI.

See each plugin's README for installation, configuration, compatibility notes, and troubleshooting.

## Mia AI recipe deployment tools

- [mia-recipe-ops](./mia-recipe-ops/README.md) — reusable operations overlay for Mia AI's TensorFold recipes on **two DGX Sparks**. Adds coordinated automatic recovery, progress monitoring, and diagnostics before recovery, with a documented profile of **4 active streams + 6 waiting requests**, a **786,432-token context window**, and conservative memory settings. Includes a stock-versus-deployed comparison, systemd service renderer, and an end-to-end CLIProxyAPI disconnect verifier.

This is a host-side deployment package, not a native CPA plugin. Inference, queued-client cancellation, and engine admission support come from the upstream Mia recipe. The documented memory profile is deployment-specific; follow the adaptation guide before using it with another recipe or model.
