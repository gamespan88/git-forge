# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/gamespan88/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/gamespan88/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge written in Go (standard library only): git smart HTTP (upload-pack protocol v2, receive-pack v0), the REST v3 API for repositories, contents, issues and pull requests, the GraphQL v4 API, and an MCP endpoint, all served from one in-memory store by a single process.

The server ships as one static binary (`forge.gz`, built from the Go sources in `src/`). The Python entrypoint decompresses it into an anonymous memfd and executes it from there, so the forge runs even on a noexec repository mount; repeat git answers (advertisements, ls-refs, fetch packs) are served from a response cache keyed by the repository's object count and refs.

## Run it

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 python3 main.py
```

Rebuild the binary from source with:

```sh
cd src && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../forge . && gzip -9 -f ../forge
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=j4y8ns9h&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
