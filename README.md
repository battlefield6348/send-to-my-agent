# send-to-my-agent

把文字透過 HTTP API 交給一個 AI agent CLI（Claude Code、Codex 或 Gemini CLI）處理，並回傳 agent 的回覆。每個服務實例（容器）綁定一個 agent。

## 以容器執行

建置時以 `AGENT` 指定要安裝的 agent（`claude`、`codex`、`gemini`）：

```sh
docker build --build-arg AGENT=claude -t send-to-my-agent:claude .
```

啟動時提供 API token、agent 憑證，並可掛載 skills 資料夾：

```sh
docker run -d -p 8080:8080 \
  -e API_TOKEN=請換成隨機長字串 \
  -e ANTHROPIC_API_KEY=... \
  -v ./skills:/skills:ro \
  send-to-my-agent:claude
```

| Agent | 憑證（擇一） |
| --- | --- |
| claude | `-e ANTHROPIC_API_KEY=...`、`-e CLAUDE_CODE_OAUTH_TOKEN=...`（`claude setup-token` 產生），或掛載 `-v <檔案>:/home/node/.claude/.credentials.json` |
| codex | `-e CODEX_API_KEY=...`，或掛載 `-v <檔案>:/home/node/.codex/auth.json`（`codex login` 產生） |
| gemini | `-e GEMINI_API_KEY=...` |

掛載憑證檔時建議掛載複本，CLI 更新 token 時會寫回該檔。

### Skills

`/skills` 下每個子資料夾即一個 skill，內含 `SKILL.md`（YAML frontmatter 的 `name`、`description` 加上說明內容）：

```text
skills/
└── secret-word/
    └── SKILL.md
```

容器內 `/skills` 已連結至該 agent 讀取 skills 的位置（claude：`~/.claude/skills`、codex：`~/.agents/skills`、gemini：`~/.gemini/skills`），agent 依 `description` 自動判斷何時使用。

## 本機直接執行

```sh
AGENT=claude API_TOKEN=請換成隨機長字串 go run .
# 預設監聽 127.0.0.1:8080，可用 ADDR 調整
```

`API_TOKEN` 未設定或 `AGENT` 不是 `claude`、`codex`、`gemini` 時服務拒絕啟動。

## API

`POST /v1/ask`

```sh
curl -H "Authorization: Bearer $API_TOKEN" \
  -d '{"prompt":"用一句話解釋 Go 的 context"}' \
  http://127.0.0.1:8080/v1/ask
```

Request：`{"prompt":"..."}`，prompt 不可為空，Body 上限 1 MB。

成功回應 `200`：`{"output":"..."}`

錯誤回應格式為 `{"error":"..."}`：

| 狀態碼 | 情境 |
| --- | --- |
| 400 | JSON 格式錯誤、prompt 為空 |
| 401 | Bearer token 缺少或錯誤 |
| 502 | agent 執行失敗，訊息附上 agent 的 stderr |
| 504 | agent 執行超過 5 分鐘 |

## 執行方式與安全性

- 每個請求啟動一次 agent CLI 的非互動模式，prompt 經由 stdin 傳入：`claude -p`、`codex exec -`、`gemini --skip-trust -p " "`。每個請求都是獨立對話。
- 用戶端中斷連線時，對應的 agent 程序會一併終止。
- 持有 `API_TOKEN` 的呼叫端等同可操作 agent，agent 讀得到容器內的檔案（包含掛載的憑證），請只把 token 交給可信任的系統。
- 權限：claude、gemini 沿用 CLI 預設權限（需核准的工具操作會被拒絕）。codex 在容器內以 `sandbox_mode = "danger-full-access"` 執行，由容器負責隔離，因為 codex 的 bubblewrap 沙箱在 Docker 預設 seccomp 下無法建立 namespace；本機直接執行時維持 codex 預設的唯讀沙箱。
