# 架構與原理

這份文件說明 send-to-my-agent 內部怎麼運作，適合第一次要修改程式的人閱讀。使用方式請看 [README](README.md)。

## 一句話說明

這個服務把「HTTP 請求裡的文字」轉交給一個在背景持續執行的 AI agent CLI（claude、codex 或 gemini），等它回答完，再把答案放進 HTTP 回應。

可以把它想成**櫃台人員**：客人（呼叫 API 的系統）把問題交給櫃台，櫃台轉給後面的專家（agent CLI），專家回答後櫃台再轉回給客人。同一位客人之後再來問，櫃台會找**同一位專家**，所以專家記得前面聊過什麼。

## 為什麼不是每次都重新執行 CLI？

最直覺的做法是每收到一個問題就執行一次 `claude -p "問題"`，但這樣有兩個問題：

1. **慢**：每次啟動 CLI 都要載入程式、讀設定、連線，常常要好幾秒。
2. **沒有記憶**：程式結束後對話內容就消失了，第二個問題不知道第一個問題問了什麼。

所以這個服務讓 CLI **一直開著**，把新的問題「寫進」它的輸入，再從它的輸出「讀回」答案。這個開著的 CLI 就叫做一個 **session**。

## 一個請求的完整旅程

以「接續既有 session 問問題」為例：

```text
呼叫端
  │  POST /v1/ask  {"session_id":"ABC","prompt":"剛剛的數字是？"}
  ▼
internal/server/server.go      ① 檢查 token、解析 JSON、設定 5 分鐘逾時
  ▼
internal/server/sessions.go    ② 用 session_id 找到 session，鎖住它（同一 session 一次只問一題）
  ▼
internal/agent/agent.go        ③ 把問題包成該 agent 看得懂的 JSON 格式
  ▼
internal/agent/process.go      ④ 把這行 JSON 寫進 CLI 的 stdin，再一行一行讀 stdout
  ▼
agent CLI（claude / codex / gemini）思考並輸出結果
  ▲
  └─ 讀到「回答結束」的訊號後，答案沿原路傳回，回應 {"session_id":"ABC","output":"7"}
```

以下依序說明每一層。

### ① server：櫃台（`internal/server/server.go`）

只負責 HTTP 相關的事：

- 比對 `Authorization: Bearer <token>`。比對時使用 `subtle.ConstantTimeCompare`，避免攻擊者從比對所花的時間猜出 token。
- 限制 body 最大 1 MB，prompt 不可為空。
- 建立一個 5 分鐘的逾時 `context`。呼叫端中途斷線或時間到，下層都會收到「停止」訊號。
- 把下層回傳的錯誤轉成 HTTP 狀態碼（404、502、504）。

### ② sessions：管理專家的名冊（`internal/server/sessions.go`）

`sessionManager` 裡有一張 `map[session_id]*session` 對照表：

- **沒帶 `session_id`**：啟動一個新的 agent CLI，產生隨機 id，登記進對照表。
- **帶了 `session_id`**：從表裡找出來，找不到就回 404。
- **一次一題**：每個 session 有自己的鎖（`sync.Mutex`），因為 CLI 同時只能處理一個問題。不同 session 之間互不影響，可以同時進行。
- **閒置關閉**：每個 session 有一個計時器（`time.AfterFunc`），每次提問完就重新計時；超過 `IDLE_TIMEOUT` 沒有新問題，就關掉 CLI 並從表裡移除。
- **出錯就關閉**：只要有一次提問失敗或逾時，就不確定 CLI 處在什麼狀態，所以直接關掉，讓呼叫端開一個新的 session。

### ③ agent：翻譯官（`internal/agent/agent.go`）

三種 CLI 都能「一直開著並接收問題」，但各自使用不同的訊息格式。這一層把三種格式統一成同一個介面：

```go
type Conversation interface {
	Ask(ctx context.Context, prompt string) (string, error)
	Close() error
}
```

上層只呼叫 `Ask`，不需要知道底下是哪一個 agent。

| Agent | 啟動指令 | 使用的協定 | 怎麼知道回答完了 |
| --- | --- | --- | --- |
| claude | `claude -p --input-format stream-json --output-format stream-json` | Claude Code 自訂的事件格式 | 收到 `"type":"result"` 事件 |
| codex | `codex app-server` | JSON-RPC 2.0 | 收到 `turn/completed` 通知 |
| gemini | `gemini --acp` | ACP（Agent Client Protocol，以 JSON-RPC 為基礎） | `session/prompt` 的回應回來 |

以 claude 為例，送進 stdin 的是一行：

```json
{"type":"user","message":{"role":"user","content":"剛剛的數字是？"}}
```

stdout 會陸續吐出好幾行事件（初始化、思考過程……），程式全部略過，直到讀到這行：

```json
{"type":"result","subtype":"success","is_error":false,"result":"7"}
```

再取出其中的 `result` 作為答案。

codex 和 gemini 開始對話前要先「打招呼」：codex 送 `initialize` 再送 `thread/start`，gemini 送 `initialize` 再送 `session/new`。這些在 session 建立時就做完，之後每次提問只要送一個請求。

### ④ process：傳話的管線（`internal/agent/process.go`）

這是最底層，負責和 CLI 程序溝通。

- 用 Go 的 `os/exec` 啟動 CLI，並接上它的 **stdin**（標準輸入）和 **stdout**（標準輸出）。就像在終端機打字給它、看它印出的文字，只是改由程式來做。
- 每則訊息都是**一行 JSON 加上換行**（這種格式稱為 NDJSON，newline-delimited JSON），所以「讀到換行」就代表讀完一則訊息。
- 一個背景 goroutine 不停地從 stdout 讀行，放進 channel；`Ask` 從 channel 一則一則取出來判斷。
- CLI 的 stderr（錯誤輸出）保留前 64 KB。CLI 意外結束時，這段內容會放進錯誤訊息，方便除錯。
- CLI 以獨立的 **process group** 啟動。關閉時對整個群組送 `SIGKILL`，連 CLI 自己開出來的子程序（例如 MCP server、shell）也會一起結束，不會留下殭屍程序。

codex 和 gemini 都使用 JSON-RPC，所以這個檔案裡還有共用的 `rpcConn`：

- **request**：帶有 `id`，對方回覆時會帶回同一個 `id`，用來配對「哪個回答對應哪個問題」。
- **notification**：沒有 `id`，是單向通知，例如「這裡有一段新的回答文字」。
- **agent 反過來問我們**：例如 gemini 會問「可以執行這個工具嗎？」。這個服務一律拒絕，行為和 CLI 單次執行時的預設一致。

## 在容器裡多做了什麼

`entrypoint.sh` 在服務啟動前執行，以 root 身分處理只有 root 能做的事，最後切換成 `node` 使用者執行服務：

1. 準備 `~/.claude`：設定擁有者、把 `/skills` 連結進去、寫入 `settings.json`（預先允許執行 skill 附帶的 node 腳本）。
2. 從唯讀掛載的 `/secrets` **複製**登入憑證，讓 `node` 讀得到。容器內已經有憑證時就不再複製。
3. 用 `setpriv` 切換成 `node` 使用者，執行 `agentapi`。

## 想修改時從哪裡下手

| 想做的事 | 修改的位置 |
| --- | --- |
| 新增一種 agent | `internal/agent/agent.go`：在 `definitions` 加一筆，並寫一個 `connectXxx` 函式 |
| 調整 API 欄位或錯誤碼 | `internal/server/server.go` |
| 調整 session 的生命週期 | `internal/server/sessions.go` |
| 新增環境變數 | `cmd/agentapi/main.go`，並記得更新 README 與 `docker-compose.yml` |

測試不需要真的 AI 服務：

- `internal/agent/agent_test.go` 讓測試程式**自己扮演假的 CLI**（以 `FAKE_AGENT` 環境變數切換），用真實的協定格式對話。
- `internal/server/server_test.go` 用假的 `Conversation` 測試 HTTP 和 session 的行為。

執行 `go test ./...` 即可。
