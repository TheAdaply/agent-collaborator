# Engines

An engine is any CLI that can take a task as a command-line argument and print (or write) an answer.
Agent Collaborator describes each one with a TOML file. Files in `~/.config/agent-collaborator/engines/` add engines or
override the built-ins of the same name.

```toml
name = "claude"
description = "Claude Code"

# Executable that must be on PATH (defaults to run[0]).
detect = "claude"

# Optional sign-in check: must exit 0 (and match ready_match, if set). Cached for a minute.
ready = ["claude", "auth", "status"]
ready_match = '"loggedIn":\s*true'

# The task command for your own machines, and the read-only one for teammates.
# Placeholders: {prompt} (the whole task, as one argument), {dir} (working directory),
# {out} (a file the engine may write its final answer to).
run         = ["claude", "-p", "--dangerously-skip-permissions", "{prompt}"]
run_limited = ["claude", "-p", "--permission-mode", "dontAsk", "--allowedTools", "Read,Glob,Grep", "{prompt}"]

# "stdout": the answer is what the command prints. "file": it writes {out}.
output = "stdout"

# Patterns meaning "out of usage". Checked only when the command fails, or when its whole answer
# is short, so an answer that merely mentions rate limits doesn't count.
# A named group (?P<reset>\d{10}) with a Unix time sets when the engine is back; otherwise `cooldown`.
exhausted = ['usage limit reached\|(?P<reset>\d{10})', '(?i)usage limit', 'API Error: 429']
cooldown = "1h"
```

Then put its name in the router order in `config.toml`:

```toml
engines = ["claude", "codex", "opencode"]
```

## How the router decides

For each engine in order:

1. Not installed, not signed in, out of usage, or (for a team task) no `run_limited` → skip, note why.
2. Run it in `workdir`, with `AC_TASK` and `AC_HOPS` in its environment.
3. Out-of-usage message → mark it out until the reset time, and give the task to the next engine with a
   note that files may already hold partial changes.
4. Any other failure → stop and report it (retrying a broken task elsewhere just doubles the cost).
5. Success → the answer goes back to the requester with a footer naming the engine and what was tried.

`agent-collaborator engines` shows the state; `agent-collaborator engines --reset NAME` puts an engine back after you
top up its credit.

## Examples

- [opencode](../examples/engines/opencode.toml)
- [aider](../examples/engines/aider.toml)
- [a local model through ollama](../examples/engines/ollama.toml)
