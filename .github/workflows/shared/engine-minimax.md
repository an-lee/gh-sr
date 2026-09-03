---
max-ai-credits: -1
engine:
  id: claude
  env:
    ANTHROPIC_API_KEY: ${{ secrets.MINIMAX_API_KEY }}
    ANTHROPIC_BASE_URL: "https://api.minimaxi.com/anthropic"
    ANTHROPIC_MODEL: "MiniMax-M3"
    API_TIMEOUT_MS: "3000000"
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
    CLAUDE_CODE_AUTO_COMPACT_WINDOW: "512000"
models:
  providers:
    anthropic:
      models:
        MiniMax-M3:
          cost:
            # Per-token dollar rates (gh-aw-firewall's provider overlay spec,
            # models.dev style): the api-proxy multiplies these by 1e6, so
            # $/1M list prices here would inflate AI-credit accounting ~1e6x
            # and trip the non-overridable 10,000-AIC hard cap on the first
            # request. MiniMax-M3 list pricing: $1.20/$4.80/$0.24 per 1M.
            input: "1.2e-06"
            output: "4.8e-06"
            cache_read: "2.4e-07"
network:
  allowed:
    - defaults
    - api.minimaxi.com
---
