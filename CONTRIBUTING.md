# Contributing

Thanks for helping improve Agent Collaborator. This is early software; small, focused changes and
clear bug reports are welcome.

Build with `make`. Before opening a pull request, run `make test` with Go (the version in go.mod)
and tmux available. It checks formatting, runs vet and unit tests, and exercises two local daemons
with fake engines; no AI credit is used.

Open a PR at https://github.com/TheAdaply/agent-collaborator describing the problem, your change,
and how you checked it. Add tests when they help verify behavior. DCO/sign-off is optional.
Please report vulnerabilities privately using the guidance in [docs/security.md](docs/security.md).
