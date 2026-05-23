# STATUS — claude-knowledge-vault (1차 마무리)

> 2026-05-23 기준. PoC v1 구현 phase 의 1차 마무리 시점. 이 문서는
> 다음 세션이 컨텍스트 없이 이어받기 위한 단일 진입점이다.
> 원본 계획은 `PLAN.md` (autoplan) / `IMPL_PLAN.md` (plan-build) 그대로
> 유효. 이 STATUS 는 그 두 문서의 *현재 상태* 슬라이스다.

## 0. 한 줄 요약

`claude-knowledge-vault` PoC v1 의 **모든 critical path**(core 9/9
+ dashboard 5/5) 가 동작 + 검증된 상태. 단일 binary `kvault` 가
MCP 서버, web dashboard, 한방-CLI 의 세 surface 를 모두 제공.
남은 작업은 **packaging shim (run.sh, slash command, release
workflow)** 과 **테스트/문서 확장** 두 덩어리 — 모두 critical path
외, env-sync 패턴 fork 로 빠르게 마무리 가능.

## 1. GitHub remote

```
origin   https://github.com/0xmhha/claude-knowledge-vault.git
```

로컬 commit 들이 origin/main 으로 push 되어야 한다. 다음 세션 첫
작업이 push 라면 [§9 진입 방법](#9-다음-세션-진입-방법) 의 "remote
push" 참조.

### 1.1. 자매 plugin (fork base)

남은 task 중 일부는 `claude-env-sync` 의 동일 task 산출물을
직접 fork 한다 (PLAN §3 "Already exists" map). 새 머신에서는
sibling 으로 clone 해 두면 같은 fork-then-port 흐름을 그대로
재현할 수 있다.

```
git clone https://github.com/0xmhha/claude-env-sync.git ../claude-env-sync
```

각 task 의 spec 본문 (§5) 에 어떤 env-sync 파일을 fork 하는지 commit
SHA 와 함께 명시했으니, 위 디렉토리만 있으면 `cp` 한 차례로 끝난다.

## 2. 무엇을 만드는 중인가

- **claude-knowledge-vault**: Claude Code 가 `~/.claude/projects/*.jsonl`
  로 남기는 대화 history 를 로컬에서 인덱싱 + 자연어 검색하는 plugin.
- 자매 plugin **`claude-env-sync`** 옆에 위치 (PLAN.md §12 비교 표).
- **목표 magical moment**: "지난 주에 결정한 webhook hmac 정책 뭐였지?"
  → 3 단어 입력 → 1 초 안에 해당 turn 인용 + 파일 위치.
- 6 premise (PLAN.md §1, 모두 사용자 확정):
  1. quote, not synthesise — RAG 아님, BM25 ranked 인용
  2. single-user, single-machine (multi-user 는 별도 layer)
  3. 100 % local — embedding 채택 시 ONNX/llama.cpp 만, 외부 API 0
  4. env-sync 와 동일 stack family + sqlite 의존성만 완화
  5. `~/.claude/projects/*.jsonl` read-only walk (hook 캡쳐는 v2)
  6. 3 surface: MCP + dashboard + slash

## 3. 완료 현황 (17 commits)

```
fadcd7d  feat(dashboard): T-D.5 drawer polish — keyboard nav + surrounding turns
3bbca4a  feat(dashboard): //go:embed web/* — binary serves UI standalone   T-D.4
c648954  feat(dashboard): web app.js — vanilla ES2020, wires every hook   T-D.2
dd612c9  feat(dashboard): web HTML/CSS skeleton — search-first layout      T-D.1
127e71a  feat(dashboard): HTTP API + SSE wired to store/indexer/search     T-D.3
a932308  feat(cmd): kvault dual-mode entry — first runnable binary         T-C.9
221c76b  feat(secrets): re-mark API keys + tokens before render            T-C.6
2b085ba  feat(mcp): in-house MCP server — verbatim fork from env-sync      T-C.8
0ca652a  feat(indexer): source → chunk → store pipeline + lock             T-C.7
6f4a791  feat(chunk): heading-aware, code-block-atomic splitter            T-C.3
75059d5  feat(source): walk + tolerant jsonl decode of ~/.claude/projects  T-C.2
be69f33  feat(search): BM25 lane + snippet windowing + trigram fallback    T-C.5
3088b0a  feat(store): sqlite FTS5 vault — schema, migrations, CRUD         T-C.4
ba2b67d  chore: bootstrap claude-knowledge-vault plugin scaffolding        T-C.1
ab8c275  ci: add GitHub Actions test workflow                              T-P.1
4c843cc  docs(plan): /buddy:plan-build output — IMPL_PLAN.md (6-stage)
dbcc9ce  docs(plan): /buddy:autoplan output — PLAN.md APPROVED
```

### 트랙별 진척

| Track             | 완료 task                                                                  | Status |
| ----------------- | --------------------------------------------------------------------------- | ------ |
| core-track        | T-C.1 / T-C.2 / T-C.3 / T-C.4 / T-C.5 / T-C.6 / T-C.7 / T-C.8 / T-C.9       | **9/9 ✓** |
| dashboard-track   | T-D.1 / T-D.2 / T-D.3 / T-D.4 / T-D.5                                       | **5/5 ✓** |
| packaging-track   | T-P.1                                                                       | 1/4    |
| docs-test-track   | —                                                                           | 0/6    |

### Critical path (IMPL_PLAN §3)

`T-C.1 → T-C.4 → T-C.5 → T-C.7 → T-C.9 → T-D.3 → T-D.5` — **7/7 done**.
critical path 외 모든 task 가 W-2 (AI agent) 분배 가능.

## 4. 측정값 (2026-05-23)

### 패키지별 커버리지

| Package              | Coverage | 참고                                                    |
| -------------------- | -------- | ------------------------------------------------------- |
| internal/secrets     | 97.9 %   |                                                         |
| internal/chunk       | 96.5 %   |                                                         |
| internal/mcp         | 91.8 %   | env-sync 와 동일 (verbatim fork)                        |
| internal/source      | 90.7 %   |                                                         |
| internal/search      | 89.2 %   |                                                         |
| internal/dashboard   | 83.3 %   |                                                         |
| internal/indexer     | 82.1 %   |                                                         |
| **internal/store**   | **68.3 %** | **회귀 — 아래 §6 참조**                              |
| cmd/kvault           | 34.8 %   | thin glue, AC band 충족                                 |

내부 8 패키지 평균 (cmd 제외) **87.6 %**. PLAN §8 목표 80 % 충족
(store 만 회귀 — 회복 procedure 는 §6).

### 코드량

| 영역                     | LoC   | 비고                                  |
| ------------------------ | ----- | ------------------------------------- |
| Go production            | 3 121 | 8 internal + 1 cmd                    |
| Go tests                 | 2 821 | unit + integration mix                |
| Web (HTML + CSS + JS)    |   798 | 115 + 177 + 506                       |
| Bash                     |     0 | T-P.2 / T-T.3 / T-T.4 미작성          |
| **합계 (코드 + 테스트)** | **~6 740** | env-sync (6 750) 와 거의 일치     |

### CI

`.github/workflows/test.yml` 가 env-sync 와 동일 4 job (go matrix
ubuntu+macos / bash regression / gitleaks / shellcheck) 보유. 첫
PR / push 시 자동 실행. bash regression job 은 `tests/regression-*.sh`
glob — 우리 repo 에 아직 그 파일 없음 (T-T.3 / T-T.4 가 작성).

## 5. 남은 작업 (인수인계)

PLAN / IMPL_PLAN 의 task 24 개 중 **9 개 남음**. 각 항목의
`Specification` 만 읽고 바로 구현 가능하게 작성. 권장 순서는
critical path 외 의존성 그래프 기준.

### ▶ T-P.2 — `bin/run.sh` (plugin entrypoint)

- **Priority**: High — plugin install path 의 마지막 빈 칸.
- **Estimated**: ~200 LoC bash, 30 분 (env-sync verbatim fork 수준).
- **Files**: `bin/run.sh` (mode 0755).
- **Dependencies**: T-C.9 ✓ (CLI flag freeze 됨).

**Specification (env-sync `bin/run.sh` commit `31029d9` shape)**

`.claude-plugin/plugin.json` 이 이미 `bash ${CLAUDE_PLUGIN_ROOT}/bin/run.sh --mcp` 호출. run.sh 가 다음 path resolution:

1. platform detect (darwin/linux × amd64/arm64; WSL은 linux)
2. cache: `${CLAUDE_PLUGIN_DATA}/bin/<platform>/kvault` (없으면 `${XDG_CACHE_HOME}/claude-knowledge-vault`)
3. cached binary 있으면 exec
4. 없으면:
   - `ENV_SYNC_RELEASE` 설정 → release fetch + cosign verify + extract
   - 아니면 source build (`cd $PLUGIN_ROOT/go && go build`)
5. exec with forwarded args

env-sync run.sh 의 path / binary name 만 `env-sync` → `kvault` 로 교체, repo URL `wm-it/claude-env-sync` → `0xmhha/claude-knowledge-vault` 로 교체. 나머지는 verbatim.

**Acceptance**
- Cold cache → source build path 실행, `kvault --version` 출력
- Warm cache → no rebuild log, 즉시 exec
- `ENV_SYNC_PLATFORM=linux-amd64` → cross-compile lane
- `bash -n bin/run.sh` clean, CI shellcheck 통과

### ▶ T-P.3 — `slash/kv.md` (slash command)

- **Priority**: High — Claude Code 안에서 `/kv <query>` 호출 가능.
- **Estimated**: ~50 LoC markdown, 15 분.
- **Files**: `slash/kv.md`, `.claude-plugin/plugin.json` (slash 등록).
- **Dependencies**: T-C.8 ✓ (MCP tool name 고정됨).

**Specification**

`slash/kv.md` body 는 MCP tool 을 직접 호출하는 한 문단 + 인자
forwarding 패턴. Claude Code 의 slash command 가 본문을 system
prompt 로 주입.

대략:

```markdown
---
description: Search past Claude Code conversation history
---

Use the `kv_search` MCP tool with `{query: "$ARGUMENTS"}`. Format the
top 5 results as a bullet list, each line showing `session_id`,
`turn_index`, role, and the 240-char snippet. If `$ARGUMENTS` is
empty, ask the user for a query.
```

`plugin.json` 에 `"commands": [{"name": "kv", "file": "slash/kv.md"}]` 추가.

**Acceptance**
- `/plugin install` 후 Claude Code 안에서 `/kv webhook signing` 시 `kv_search` MCP call 발생
- 결과가 bullet list 로 화면에 표시

### ▶ T-P.4 — Release workflow (cosign keyless OIDC)

- **Priority**: Medium — release-fetch path 검증 가능. source build
  이미 동작이라 plugin 사용 자체에는 blocker 아님.
- **Estimated**: ~150 LoC YAML, half-day (env-sync 의 동명 task 가
  미완 — 우리는 그것까지 새로 짜야 함).
- **Files**: `.github/workflows/release.yml`.
- **Dependencies**: 없음 (T-C.9 binary, T-P.2 run.sh 있으면 즉시 가능).

**Specification**

1. Trigger: tag push `v*`.
2. Matrix: 4 platform (`darwin-amd64`, `darwin-arm64`, `linux-amd64`, `linux-arm64`).
3. Build: `cd go && GOOS=$os GOARCH=$arch go build -ldflags "-X main.Version=$TAG" -o kvault ./cmd/kvault`
4. Tar: `tar -czf kvault-$TAG-$os-$arch.tar.gz kvault`
5. Sign: `cosign sign-blob --yes <archive>` (keyless OIDC, no key)
6. Checksums: `sha256sum kvault-*.tar.gz > checksums.txt`
7. Upload to GitHub Release: 모든 tarball + `.sig` + `checksums.txt`.
8. permissions: `id-token: write`, `contents: write`.

**Acceptance**
- 테스트 tag `v0.0.1-rc1` push → 4 tarball + 4 .sig + checksums.txt 가 Release page 에 보임
- `bin/run.sh` 에 `ENV_SYNC_RELEASE=v0.0.1-rc1` 설정 후 fresh 머신에서 fetch + verify + exec 성공
- `cosign verify-blob` 가 `--certificate-identity-regexp 'https://github.com/0xmhha/claude-knowledge-vault/'` 로 succeeds

### ▶ T-T.3 — `tests/regression-no-network.sh`

- **Priority**: High — P3 (100 % local) 주장의 mechanical proof.
- **Estimated**: ~80 LoC bash, 1 hr.
- **Files**: `tests/regression-no-network.sh` (+x).
- **Dependencies**: T-C.9 ✓.

**Specification**

`unshare -n` (Linux) 또는 `nettest` 로 모든 egress 차단 환경에서:
1. fresh tempdir
2. `kvault --once index --plugin-data $TMP --root $FIXTURES` 성공
3. `kvault --once search --plugin-data $TMP --query foo` 성공
4. 어떤 단계에서든 outbound socket 시도 시 assertion fail

env-sync `tests/regression-secrets-leak.sh` 의 assertion style fork.
CI 의 `bash-regression` job 이 `tests/regression-*.sh` glob 으로
자동 pickup. macOS dev 에서는 `unshare -n` 없음 — Linux CI 만 trust,
README 에 명시.

**Acceptance**
- Linux CI 에서 exit 0 + "0 egress attempts" assertion

### ▶ T-T.4 — `tests/regression-secret-rerender.sh`

- **Priority**: High — C7 (default-on secret masking) 의 mechanical proof.
- **Estimated**: ~100 LoC bash, 1 hr.
- **Files**: `tests/regression-secret-rerender.sh` (+x).
- **Dependencies**: T-D.3 ✓ + T-D.5 ✓ + T-C.6 ✓.

**Specification**

1. fresh tempdir + 6 family canary 가 박힌 fake jsonl seed
   (`sk-ant-CANARY-…`, `AKIA…`, JWT 모양, ghp_…, postgres://user:pass@host, private key PEM)
2. `kvault --once index ...` → DB 채움
3. `kvault --port 0 ...` background
4. `curl /api/search?query=…` 호출 후 모든 canary 가 응답 본문에 부재하고 `REDACTED` 가 존재함을 grep
5. `curl /api/turn?...` 도 동일 검증
6. kill server, cleanup, exit 0

env-sync `tests/regression-secrets-leak.sh` (commit `1a71f79`) 의
end-to-end assertion 패턴 그대로 (build → seed → run → grep).

**Acceptance**
- 13+ assertion 전수 통과 + 6 canary 전부 마스킹 확인

### ▶ T-T.5 — `README.md` (purpose + install + ASCII screenshot)

- **Priority**: High — fresh reader 가 "clone → build → first search"
  까지 갈 수 있는 단일 문서.
- **Estimated**: ~280 LoC markdown, 1 hr.
- **Files**: `README.md`.
- **Dependencies**: T-C.9 ✓ + T-D.3 ✓ + T-P.2 (run.sh 후 install
  안내 정확해짐).

**Specification (env-sync README.md commit `d3b87ad` shape)**

- Status 표 (track 별 진척 — 이 STATUS.md 의 §3 그대로)
- ASCII architecture sketch (env-sync README 의 sketch 변형)
- Source-build quick start (5 명령으로 first search, TTHW 90 s 목표)
- ASCII dashboard screenshot (T-D.5 의 drawer + filter + result 포함)
- 3 reference 표: CLI flags / MCP tools / Dashboard endpoints
- Sanitize/exclude policy 표 (`internal/secrets/patterns.go` 매핑)
- Security 섹션 — `tests/regression-no-network.sh` + `secret-rerender.sh` 가 living spec
- 로드맵 표

**Acceptance**
- README install 명령 그대로 clean 머신에서 실행 → first search 까지 <90 s
- dangling link 0
- `wc -l README.md` ≈ 280

### ▶ T-T.6 — `docs/{GETTING_STARTED,SEARCH_TIPS,SECURITY,BUILD}.md`

- **Priority**: Medium — README 만 으로 onboarding 가능하지만 docs
  가 있으면 매끄러움.
- **Estimated**: ~400 LoC, half-day.
- **Files**: `docs/GETTING_STARTED.md`, `docs/SEARCH_TIPS.md`, `docs/SECURITY.md`, `docs/BUILD.md`.
- **Dependencies**: T-T.5 ✓ (README 가 docs 로 link).

**Specification (간략)**

- `GETTING_STARTED.md` — first-search walkthrough + screenshot, two-machine 사용 흐름 (env-sync 의 vault.db sync layer 안내)
- `SEARCH_TIPS.md` — FTS5 MATCH 문법 cheatsheet (phrase 인용, NEAR, prefix `*`)
- `SECURITY.md` — threat model + cosign verify procedure + secrets policy
- `BUILD.md` — Go 1.25.2 install, per-package 안내, test 명령, pre-commit hook 활성화

**Acceptance**
- 4 파일 모두 존재, README 에서 link 깨지지 않음, SECURITY 의 cosign block 이 `bin/run.sh` 의 identity regex 와 byte-exact

### ▶ T-T.1 / T-T.2 — 테스트 보강 (selective)

- **Priority**: Low — 8 internal 패키지 평균 87.6 % cov, store 회귀
  (68.3 %) 만 회복하면 모두 80 %+. 추가 보강은 marginal.
- **Estimated**: 1–2 hr opportunistic.
- **Files**: 기존 `*_test.go` 에 추가.

**즉시 회복할 것 (next 세션 첫 작업 후보)**
- `store.GetTurn` + `store.GetChunksByTurn` unit test 추가 — T-D.5
  에서 새 helper 추가했지만 store_test 미보강 → cov 82 → 68 %.
  2–3 test 만 추가하면 80 %+ 회복.

**T-T.2 integration suite**
- T-T.4 (secret-rerender) + T-T.3 (no-network) 가 이미 end-to-end
  검증. 추가 integration 분리는 documentation 가치만.

## 6. 알려진 todo / 보류 결정

1. **store coverage 회귀 (82 % → 68.3 %)** — T-D.5 commit `fadcd7d`
   에서 `GetTurn` + `GetChunksByTurn` 두 helper 추가했지만 store
   unit test 안 보강. 다음 세션 첫 작업: 두 helper 의 `t.TempDir()`
   기반 unit test 2~3 개 추가.
2. **`bin/run.sh` race smoke** — 1차 작업 중 background `sleep 1.5`
   + `grep` 가 buffered server log 못 잡아 race 발생. 다음 smoke
   에서 `until grep -q ... ; do sleep 0.1; done` 패턴으로 견고화.
3. **Dogfood 안 함** — 실제 `~/.claude/projects` 인덱싱 + 검색을
   T-T.5 작성 전 한 차례 해보면 README screenshot 가 사실에 가까워짐.
4. **Hook 기반 실시간 캡쳐 (TK-B)** — v2. v1 은 jsonl 일괄 walk.
5. **sqlite-vec + embedding (TK-A)** — v1.1. BM25 가 사용 query
   class 대부분을 커버한다고 실측 후 결정.
6. **multi-machine vault.db sync (TK-E)** — env-sync 를 layer.
7. **Encryption-at-rest (TK-H)** — 사용자 filesystem 신뢰 (~/.claude/
   가 이미 `.credentials.json` 등을 가지고 있음).
8. **General-file `${HOME}` substitution** — env-sync 가 PoC v1 에서
   skip 한 같은 결정. knowledge-vault 도 동일.
9. **LSP workspace 경고** — `go.work` 부재로 multi-module 환경에서
   `gopls` 가 못 잡음. 빌드/테스트 무관. 다음 세션이 IDE 사용 시
   `harness/` root 에 `go.work` 작성하면 해결 (선택).
10. **`navigator.clipboard.writeText`** — HTTPS / localhost 에서만
    작동. 우리 127.0.0.1 binding 이라 OK; reverse proxy 도입 시 깨짐.

## 7. Decision Audit Trail 참조

23 row PLAN.md §15 + 본 STATUS 의 모든 변경 결정이 commit body 에
명시. 별도 audit-trail.md 안 작성 (env-sync 와 동일 방침).

## 8. 사전 메모 (다음 세션에 도움)

- **fork-then-port 전략**: env-sync 의 모든 lint config / hook /
  CI / mcp 패키지 / dashboard 패턴 / run.sh 가 path/binary name
  치환만으로 재사용 가능. 새 작업 진입 전 env-sync 의 동일 task
  commit 을 reference 로 잡으면 50 % 시간 절감.
- **`modernc.org/sqlite` 가 유일한 외부 deps** (v1.50.1). FTS5
  `tokenize='porter unicode61'` + `tokenize='trigram'` 모두 default
  enabled — 별도 build tag 불필요.
- **JSON 키 컨벤션**: store.* 응답 (Session/Turn/Stats/SearchResult)
  은 Go struct tag 없음 → PascalCase. dashboard 응답
  (turnResponse/indexResponse/...) 은 `json:"..."` 로 camelCase.
  대시보드 JS 가 `?? sessions` 같은 fallback 으로 양쪽 다 받음.
- **Lock file**: env-sync `acquireLock` 패턴 그대로 port. 5 min
  stale TTL, RFC3339 mtime.
- **Secret masking**: search snippet + turn content 양쪽에서 `Mask`
  호출 가능 (idempotent — 마스킹된 텍스트에는 secret regex 재매치
  안 됨).
- **Web smoke race**: background `&` + `wait` + curl loop 시
  `until` 패턴 사용 (`until curl -s … >/dev/null; do sleep 0.1; done`).
  PoC 작업 중 한 번 race 발생 — fix 패턴 README 에도 명시 권장.

## 9. 다음 세션 진입 방법

### 9.0. Toolchain (cold install on a new machine)

| 도구 | 버전 | 비고 |
| --- | --- | --- |
| Go | **1.25.2** 이상 | go.mod 에 pin. 더 낮으면 build 실패 |
| modernc.org/sqlite | v1.50.1 | go.sum pin, 첫 `go build` 시 10–30 초 download |
| golangci-lint | latest (≥ 1.61) | CI 동일. `brew install golangci-lint` 또는 [공식 install script](https://golangci-lint.run/welcome/install/) |
| gitleaks | optional | 없으면 pre-commit 의 fallback grep 사용 |
| shellcheck | CI 필수 | T-P.2 / regression 스크립트 lint |
| node | optional | T-D.2 / T-D.5 의 `node --check app.js` |
| sqlite3 CLI | optional | DB 직접 디버그 |

### 9.1. Setup (clone 후 한 번만)

```bash
git clone https://github.com/0xmhha/claude-knowledge-vault.git
cd claude-knowledge-vault

# Sibling fork base — §5 의 fork-then-port task 들이 이걸 참조
git clone https://github.com/0xmhha/claude-env-sync.git ../claude-env-sync

# pre-commit / commit-msg / pre-push hook 활성화 (필수)
git config core.hooksPath .githooks

# Commit 시 사용할 user identity (env-sync 와 동일)
git config user.name  'wm-it'
git config user.email 'wm-it@local'   # 또는 본인 이메일

# Commit message 규칙: Conventional Commits 강제
#   <type>(<scope>): <subject>
#   type:  feat | fix | refactor | docs | test | chore | perf | ci | build
#   scope: snake-case 또는 한 단어, '/' 사용 금지
# 예: feat(store): add GetTurn helper
#     ✗ feat(dashboard/web): ...    (commit-msg hook 가 reject)
```

### 9.2. Verify (clone 후 매번 — "everything still green?")

```bash
cd go && go test -race -cover ./... && go vet ./... && cd ..
golangci-lint run go/... --timeout=2m
```

기대값: 9 package 전부 green, lint 0 issues, 평균 cov 80%+
(단 store 만 회귀 68.3% — §6 참조, 다음 세션 첫 작업 후보).

### 9.3. Binary smoke (build + 4 endpoint 확인)

```bash
( cd go && go build -o ../kvault ./cmd/kvault )
mkdir -p /tmp/kv-demo
./kvault --version                                            # → 0.1.0-dev
./kvault --once stats --plugin-data /tmp/kv-demo              # → {Sessions:0, Turns:0, Chunks:0}
./kvault --once search --plugin-data /tmp/kv-demo --query x   # → []

# Dashboard (printed URL 을 브라우저로)
./kvault --port 0 --plugin-data /tmp/kv-demo
# Ctrl-C to stop
```

### 9.4. Dogfood (실제 `~/.claude/projects` 인덱싱 — 선택)

```bash
./kvault --once index --plugin-data /tmp/kv-demo
./kvault --once search --plugin-data /tmp/kv-demo --query "hook decision"
```

### Remote push (필요 시)

```bash
cd claude-knowledge-vault
git remote add origin https://github.com/0xmhha/claude-knowledge-vault.git  # 이미 있다면 skip
git push -u origin main
```

푸시 인증 실패 시: `gh auth login` 또는 GitHub PAT credential helper.

### 9.5. 환경 변수 reference

CLI flag 가 모든 옵션의 1차 source. 환경 변수는 fallback:

| Variable | 적용 | 우선순위 |
| --- | --- | --- |
| `KVAULT_DATA` | `--plugin-data` 대체 | 2 |
| `CLAUDE_PLUGIN_DATA` | `--plugin-data` 2차 fallback | 3 |
| `CLAUDE_HOME` | `--root` (자동 `/projects` 추가) | 2 |
| `KVAULT_PORT` | `--port` 대체 (dashboard 모드) | 2 |
| `CLAUDE_PLUGIN_ROOT` | plugin install 위치 (run.sh 가 사용) | n/a |
| `ENV_SYNC_RELEASE` | T-P.2 run.sh 의 release-fetch 모드 트리거 | n/a |
| `ENV_SYNC_PLATFORM` | T-P.2 run.sh 의 cross-compile 강제 | n/a |
| `ENV_SYNC_SKIP_VERIFY` | T-P.2 run.sh 의 cosign verify 우회 (NOT recommended) | n/a |

(우선순위: flag > KVAULT_* > CLAUDE_* > built-in default)

### 9.6. 다음 task 선택 (권장 순서)

1. **store cov 회복** (10 분) — `GetTurn` + `GetChunksByTurn` unit test 2개
2. **T-T.5 README + screenshot** (1 hr) — 다음 모든 docs 의 anchor
3. **T-P.2 bin/run.sh** (30 분) — env-sync verbatim fork (sibling clone 필요)
4. **T-P.3 slash/kv.md** (15 분) — `/kv <query>` 작동
5. **T-T.3 + T-T.4 regression** (2 hr) — security/privacy mechanical proof
6. **T-P.4 release workflow** (half-day) — cosign keyless OIDC
7. **T-T.6 docs/*** (half-day) — GETTING_STARTED / SEARCH_TIPS / SECURITY / BUILD
8. **T-T.1 / T-T.2** — opportunistic, marginal

이 문서 (`STATUS.md`) 와 `IMPL_PLAN.md` 두 개만 읽으면 다음 task
진입 가능. `PLAN.md` 는 의사결정 audit trail (왜 sqlite-vec 결정인지,
왜 BM25 only v1 인지) 이 필요할 때만.
