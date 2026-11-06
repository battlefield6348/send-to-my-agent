# send-to-my-agent

把文字透過 HTTP API 交給一個 AI agent CLI（Claude Code、Codex 或 Gemini CLI）處理，並回傳 agent 的回覆。Image 內含三個 CLI，啟動時以 `AGENT` 環境變數決定使用哪一個。

## 以 Docker Compose 執行

1. 建立 `.env`：

   ```sh
   AGENT=claude            # claude、codex、gemini 擇一
   API_TOKEN=請換成隨機長字串
   IDLE_TIMEOUT=10m        # 選填，session 閒置多久後關閉，預設 10m
   GEMINI_API_KEY=         # AGENT=gemini 時填入
   ```

2. 依 `AGENT` 放入對應的登入憑證：

   ```sh
   mkdir -p secrets && chmod 700 secrets
   cp ~/.claude/.credentials.json secrets/   # AGENT=claude
   cp ~/.codex/auth.json secrets/            # AGENT=codex
   chmod 600 secrets/.credentials.json secrets/auth.json
   ```

   `secrets/` 以唯讀掛載，檔案擁有者不限。容器啟動時會把憑證複製給容器內的 `node` 使用者，執行期間更新的 token 只存在容器內；容器內已有憑證時不會再複製；容器重建後若出現「Not logged in」，請重新複製主機上最新的憑證。

   claude 也可改用 `CLAUDE_CODE_OAUTH_TOKEN`（`claude setup-token` 產生）或 `ANTHROPIC_API_KEY`，codex 可改用 `CODEX_API_KEY`，加在 `docker-compose.yml` 的 `environment` 即可。

   複製進來的 claude 憑證與主機共用同一組 token，主機刷新後容器內的會失效（「OAuth session expired」）。要讓容器自己維持登入，可在 `docker-compose.override.yml` 把只給本服務使用的主機資料夾掛到 `/home/node/.claude`，再於容器內登入一次：

   ```yaml
   services:
     agent-api:
       volumes:
         - /path/to/send-to-my-agent-claude:/home/node/.claude
   ```

   ```sh
   docker compose up -d
   docker compose exec -u node agent-api claude   # 輸入 /login，依畫面網址授權後貼回驗證碼
   ```

   容器啟動時會將該資料夾擁有者改為 `node`，並建立 skills 連結與 `settings.json`（每次啟動覆寫）。已存在的 `.credentials.json` 不會被 `secrets/` 覆蓋，所以此資料夾不要與其他服務或主機共用。

3. 啟動：`docker compose up -d --build`，服務監聽 `127.0.0.1:8080`。切換 agent 時修改 `.env` 的 `AGENT` 後執行 `docker compose up -d`。

### Skills

`skills/` 會掛載至容器的 `/skills`，每個子資料夾即一個 skill，內含 `SKILL.md`（YAML frontmatter 的 `name`、`description` 加上說明內容）：

```text
skills/
└── secret-word/
    └── SKILL.md
```

容器內 `/skills` 已連結至三個 agent 讀取 skills 的位置（claude：`~/.claude/skills`、codex：`~/.agents/skills`、gemini：`~/.gemini/skills`），agent 依 `description` 自動判斷何時使用。新增或修改 skill 後重新送出請求即生效。

只有 `skills/secret-word` 範例會納入 git，其他 skill 資料夾已被忽略。skill 需要的環境變數（例如 API token）寫在 `.env`，並在 git-ignored 的 `docker-compose.override.yml` 傳入容器，Docker Compose 會自動合併。

## 專案結構

```text
cmd/agentapi/      進入點：讀環境變數並啟動 HTTP 服務
internal/server/   HTTP API（驗證、錯誤碼）與 session 管理
internal/agent/    啟動 agent CLI，並以各自協定（stream-json、JSON-RPC、ACP）透過 stdin/stdout 對話
entrypoint.sh      容器啟動：準備 ~/.claude、複製憑證、切換為 node 使用者
```

新增 agent 只需在 `internal/agent/agent.go` 的 `definitions` 加一筆，並實作對應的 `connect` 函式。

## 本機直接執行

```sh
AGENT=claude API_TOKEN=請換成隨機長字串 go run ./cmd/agentapi
# 預設監聽 127.0.0.1:8080，可用 ADDR 調整
```

`API_TOKEN` 未設定或 `AGENT` 不是 `claude`、`codex`、`gemini` 時服務拒絕啟動。

## API

`POST /v1/ask`

```sh
# 開新對話（不帶 session_id）
curl -H "Authorization: Bearer $API_TOKEN" \
  -d '{"prompt":"記住數字 7"}' \
  http://127.0.0.1:8080/v1/ask
# → {"session_id":"PWDYQBUCBFBGTMSZF5KVZQGIO4","output":"好的"}

# 接續同一段對話
curl -H "Authorization: Bearer $API_TOKEN" \
  -d '{"session_id":"PWDYQBUCBFBGTMSZF5KVZQGIO4","prompt":"剛剛的數字是？"}' \
  http://127.0.0.1:8080/v1/ask
# → {"session_id":"PWDYQBUCBFBGTMSZF5KVZQGIO4","output":"7"}
```

| Request 欄位 | 說明 |
| --- | --- |
| `prompt` | 必填，不可為空，Body 上限 1 MB |
| `session_id` | 選填。不帶時開新對話；帶入先前回傳的值則接續該對話 |

成功回應 `200`：`{"session_id":"...","output":"..."}`

錯誤回應格式為 `{"error":"..."}`：

| 狀態碼 | 情境 |
| --- | --- |
| 400 | JSON 格式錯誤、prompt 為空 |
| 401 | Bearer token 缺少或錯誤 |
| 404 | `session_id` 不存在或已閒置關閉，請不帶 `session_id` 開新對話 |
| 502 | agent 啟動或執行失敗，該 session 隨之關閉 |
| 504 | 單次回答超過 5 分鐘，該 session 隨之關閉 |

## Session 運作方式

- 每個 session 對應一個常駐的 agent 程序，同一 session 的後續提問直接送入該程序，省去每次啟動 CLI 的時間並保留對話記憶。
- 同一 session 的請求依序處理，一次一個；不同 session 可同時進行。
- session 在 `IDLE_TIMEOUT`（預設 10 分鐘）內沒有新提問即關閉並結束 agent 程序。
- 各 agent 的常駐模式：claude `-p --input-format stream-json`、codex `app-server`、gemini `--acp`。

## 安全性

- 持有 `API_TOKEN` 的呼叫端等同可操作 agent，agent 讀得到容器內的檔案（包含掛載的憑證），請只把 token 交給可信任的系統。
- 權限：claude 沿用 CLI 預設權限（需核准的工具操作會被拒絕），但預先允許 `node /skills/*`，讓 skill 附帶的腳本可以執行（skill 的 `allowed-tools` 在此模式下會導致 skill 載入失敗，請勿使用），gemini 的工具核准請求一律拒絕。codex 在容器內以 `sandbox_mode = "danger-full-access"` 執行，由容器負責隔離，因為 codex 的 bubblewrap 沙箱在 Docker 預設 seccomp 下無法建立 namespace；本機直接執行時維持 codex 預設的唯讀沙箱。
