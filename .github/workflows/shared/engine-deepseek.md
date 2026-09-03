---
max-ai-credits: -1
engine:
  id: claude
  env:
    ANTHROPIC_API_KEY: ${{ secrets.DEEPSEEK_API_KEY }}
    ANTHROPIC_BASE_URL: "https://api.deepseek.com/anthropic"
    ANTHROPIC_MODEL: "deepseek-v4-flash"
    # CLAUDE_CODE_EFFORT_LEVEL: "max"
    API_TIMEOUT_MS: "3000000"
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
models:
  providers:
    anthropic:
      models:
        deepseek-v4-flash:
          cost:
            # Per-token dollar rates (gh-aw-firewall overlay spec, models.dev
            # style) — NOT $/1M list prices; see engine-minimax.md. DeepSeek
            # list pricing: $0.14/$0.28/$0.0028 per 1M.
            input: "1.4e-07"
            output: "2.8e-07"
            cache_read: "2.8e-09"
network:
  allowed:
    - defaults
    - api.deepseek.com
---
