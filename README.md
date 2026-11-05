# send-to-my-agent

把文字透過 HTTP API 交給本機的 AI agent CLI（Claude Code、Codex、Gemini CLI）處理，並回傳 agent 的回覆。

## 啟動

前置條件：主機已安裝並登入要使用的 CLI（`claude`、`codex`、`gemini`）。

```sh
API_TOKEN=請換成隨機長字串 go run .
# 預設監聽 127.0.0.1:8080，可用 ADDR=0.0.0.0:8080 對外開放
```

未設定 `API_TOKEN` 時服務會拒絕啟動。

## API

`POST /v1/ask`

```sh
curl -H "Authorization: Bearer $API_TOKEN" \
  -d '{"agent":"claude","prompt":"用一句話解釋 Go 的 context"}' \
  http://127.0.0.1:8080/v1/ask
```

Request：

| 欄位 | 說明 |
| --- | --- |
| `agent` | `claude`、`codex`、`gemini` 擇一 |
| `prompt` | 要傳給 agent 的文字，不可為空，Body 上限 1 MB |

成功回應 `200`：

```json
{"agent":"claude","output":"..."}
```

錯誤回應格式為 `{"error":"..."}`：

| 狀態碼 | 情境 |
| --- | --- |
| 400 | JSON 格式錯誤、不支援的 agent、prompt 為空 |
| 401 | Bearer token 缺少或錯誤 |
| 502 | agent 執行失敗，訊息附上 agent 的 stderr |
| 504 | agent 執行超過 5 分鐘 |

## 執行方式與安全性

- 每個請求啟動一次 agent CLI 的非互動模式，prompt 經由 stdin 傳入：`claude -p`、`codex exec -`、`gemini --skip-trust -p " "`。
- agent 在服務的工作目錄中執行，並沿用各 CLI 預設的權限模式（需要核准的工具操作會被拒絕）。請勿以放寬權限的參數（如 `--yolo`、`--dangerously-skip-permissions`）啟動，因為呼叫端等同可在主機上操作 agent。
- 用戶端中斷連線時，對應的 agent 程序會一併終止。
