#!/usr/bin/env bash
# End-to-end test: two real agent-collaborator daemons ("alpha" and "beta") in local network mode, with fake
# engines - "claude" is always out of usage, "codex" answers - so no AI credit is spent.
# Covers: router fallback, machine asks, session inbox + reply, tmux delivery, offline queueing,
# MCP tools, engine cooldown reporting.   Usage: tests/e2e.sh [path/to/agent-collaborator]
set -uo pipefail
BIN=$(realpath "${1:-bin/agent-collaborator}")
T=$(mktemp -d /tmp/agent-collaborator-e2e.XXXX)
FAKE=$T/bin; mkdir -p "$FAKE"
PASS=0 FAIL=0
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done; tmux -L cwe2e kill-server 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT

ok()   { echo "  PASS  $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL  $1"; echo "        $2" | head -5; FAIL=$((FAIL+1)); }
check() { local name=$1 want=$2 got=$3; if grep -qE -- "$want" <<<"$got"; then ok "$name"; else bad "$name" "wanted /$want/, got: $got"; fi; }

# Fake engines.
cat > "$FAKE/fake-claude" <<'EOF'
#!/bin/sh
echo "Claude AI usage limit reached|4102444800" >&2; exit 1
EOF
cat > "$FAKE/fake-codex" <<'EOF'
#!/bin/sh
# codex-style: answer goes to the -o file (the argument after -o); the prompt is last.
out=""; prev=""; for a in "$@"; do [ "$prev" = "-o" ] && out=$a; prev=$a; done
eval last=\${$#}
printf 'codex answer: %s\n' "$(printf '%s' "$last" | tail -n 1)" > "$out"
EOF
chmod +x "$FAKE"/*
export PATH=$FAKE:$PATH

node() { # node NAME PORT PEERNAME PEERPORT
  local h=$T/$1; mkdir -p "$h/engines"
  cat > "$h/config.toml" <<EOF
name = "$1"
network = "local"
listen = "127.0.0.1:$2"
engines = ["claude", "codex"]
workdir = "$T"
[peers]
$3 = "127.0.0.1:$4"
EOF
  cat > "$h/engines/claude.toml" <<'EOF'
name = "claude"
run = ["fake-claude", "{prompt}"]
run_limited = ["fake-claude", "{prompt}"]
exhausted = ['usage limit reached\|(?P<reset>\d{10})', '(?i)usage limit']
EOF
  cat > "$h/engines/codex.toml" <<'EOF'
name = "codex"
run = ["fake-codex", "-o", "{out}", "{prompt}"]
run_limited = ["fake-codex", "-o", "{out}", "{prompt}"]
output = "file"
EOF
}
on() { local n=$1; shift; AC_HOME=$T/$n timeout 60 "$BIN" "$@"; }
start() { AC_HOME=$T/$1 "$BIN" up >"$T/$1.log" 2>&1 & pids+=($!); for _ in $(seq 50); do [[ -S $T/$1/state/agent-collaborator.sock ]] && return; sleep 0.1; done; echo "daemon $1 did not start"; cat "$T/$1.log"; exit 1; }

node alpha 17431 beta 17432
node beta 17432 alpha 17431

echo "router, no daemon"
out=$(on alpha run "say hi" 2>&1)
check "claude out of usage -> codex answers" "codex answer: say hi" "$out"
check "the fallback is reported" "claude: exhausted" "$out"
out=$(on alpha engines 2>&1)
check "cooldown uses the reset time from the error" "claude +out of usage until .*2100" "$out"

start alpha; start beta
echo "two machines"
out=$(on alpha who 2>&1)
check "who lists both machines" "beta - online" "$out"
check "who shows beta's usable engines (its own claude never ran out)" "beta - online - engines: claude, codex" "$out"

out=$(on alpha ask beta "count the files" 2>&1)
check "ask a machine: beta's router answers" "codex answer: count the files" "$out"
check "the answer says who answered" "answered by codex on beta \(claude: exhausted" "$out"

on beta register worker --inbox --agent codex >/dev/null
( on alpha ask beta/worker "what is 6*7?" > "$T/ask.out" 2>&1 ) & askpid=$!
for _ in $(seq 30); do inbox=$(on beta inbox worker); grep -q agent-collaborator-message <<<"$inbox" && break; sleep 0.3; done
check "ask a session: lands in its inbox, framed as peer input" 'agent-collaborator-message id=.* from="alpha".* trust="owner"' "$inbox"
id=$(grep -oE 'id="[^"]+"' <<<"$inbox" | head -1 | cut -d'"' -f2)
out=$(on beta reply "$id" "42" 2>&1)
check "reply is accepted" "replied" "$out"
wait $askpid; out=$(cat "$T/ask.out")
check "the asker gets the reply" "^42$" "$out"
out=$(on beta reply "$id" "again" 2>&1)
check "an ask can't be answered twice" "no pending ask" "$out"

echo "tmux delivery"
tmux -L cwe2e new-session -d -s agent -x 220 -y 30 "bash -c 'while IFS= read -r -s l; do echo \"got \${#l} chars\"; done'"
on beta register live --tmux cwe2e:agent >/dev/null
out=$(on alpha send beta/live "deploy notes are in docs/deploy.md" 2>&1)
check "typed into the live session" "delivered: typed into live" "$out"
sleep 1; pane=$(tmux -L cwe2e capture-pane -p -t agent)
check "the session received one submitted line" "got [0-9]+ chars" "$pane"

echo "offline queueing"
kill "${pids[1]}"; wait "${pids[1]}" 2>/dev/null; rm -f "$T/beta/state/agent-collaborator.sock"
out=$(on alpha send beta/worker "you were offline" 2>&1)
check "message to an offline machine is queued" "queued" "$out"
start beta
for _ in $(seq 40); do inbox=$(on beta inbox worker); grep -q "you were offline" <<<"$inbox" && break; sleep 1; done
check "queued message arrives when the machine is back" "you were offline" "$inbox"

echo "MCP"
mcp_out=$(printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"to":"beta","message":"summarize README"}}}' \
  '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"who","arguments":{}}}' \
  | AC_HOME=$T/alpha timeout 30 "$BIN" mcp --agent claude --session tester)
check "MCP initialize" '"serverInfo":\{"name":"Agent Collaborator"' "$mcp_out"
check "MCP lists the tools" '"name":"check_inbox"' "$mcp_out"
check "MCP send waits for and returns the answer" 'Answer from beta.*codex answer: summarize README' "$mcp_out"
# The MCP's own who call (while it is alive) shows the session it registered.
check "the MCP session registered itself" 'alpha/tester  \(claude, inbox' "$mcp_out"
# And it unregisters when the MCP server exits (its stdin closed above).
out=$(on alpha who 2>&1)
check "the session is gone after the MCP server exits" 'alpha \(this machine\)' "$out"
if grep -q 'alpha/tester' <<<"$out"; then bad "session cleaned up on exit" "tester still listed: $out"; else ok "session cleaned up on exit"; fi

echo "audit log"
out=$(on beta log -n 50 2>&1)
check "audit log records routed tasks" "routed:codex" "$out"

echo
echo "e2e: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
