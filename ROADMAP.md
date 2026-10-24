# Roadmap

Early v0.1 targets Linux and macOS. Engine fallback, local messaging, sessions,
MCP tools, and fleet inventory work in the local test suite. Cross-machine tailnet
operation and real engine behavior still need broader validation.

- **Durable task state:** persist task lifecycle and recovery information so daemon
  restarts do not lose the relationship between a request and its result.
- **Truthful health:** distinguish reachability, agent readiness, and inventory
  failures; retain evidence of known agents without guessing from tailnet presence.
- **Structured Codex adapter:** consume structured events and outcomes instead of
  relying on text patterns for authentication, exhaustion, and task completion.
- **Teammate onboarding:** make trust configuration, engine setup, and the first
  cross-machine exchange easier to understand and verify.
