# M3U8 網頁偵測與下載工具

下載核心位於 `downloader.go`，播放清單與解密邏輯位於 `playlist.go`，CLI 入口位於 `main.go`。程式可直接下載 M3U8，或透過 Chrome 監聽網頁請求，收集所有偵測到的 M3U8 網址後逐一下載、解密 AES-128 切片，並使用 FFmpeg 轉封裝成 MP4。

## 主要功能

- 輸入網頁網址，自動收集等待期間出現的多個 M3U8 網址並去除重複。
- 收集完成後逐一下載影片，每個影片內的切片可並行下載。
- 保留直接輸入 M3U8 網址的操作方式，也能手動指定輸入類型。
- 解析相對路徑、子播放清單與 AES-128 金鑰；資源網址未帶查詢參數時，沿用播放清單的參數。
- 依切片順序合併，以 FFmpeg 複製音視訊串流轉封裝為 MP4，不重新編碼。
- 多筆結果自動加上編號，並建立輸出目錄。
- 單筆下載失敗後繼續處理其他項目，最後顯示成功與失敗數量。

## Web 介面：啟動與操作

CLI 與 Web 是兩種獨立啟動模式。**未加 `-web` 就使用 CLI**，不需要啟動 Web、不讀取 Web 的 `envfile` 或網站設定，也不需要先執行 `-init-config`。

Web 使用 `github.com/asccclass/sherryserver`，前端內嵌於執行檔，無須 Node.js 或 CDN。進度經原生 WebSocket 推送，包含偵測、解析、切片下載／解密、合併及 FFmpeg 轉封裝。

首次建立個人的隨機設定金鑰，再啟動服務：

```powershell
go run . -init-config
go run . -web
```

開啟 **http://127.0.0.1:8080**。`-init-config` 只建立尚不存在的 `envfile`，不覆寫既有設定；若已有自己的 `envfile`，確認 `SessionCookieAuthKey` 至少 32 bytes 且非上游舊預設值即可。環境變數優先於檔案，空白環境變數也可能阻止載入檔案中的同名值。

也可使用編譯後的程式：

```powershell
go build -o m3u8-dl.exe .
.\m3u8-dl.exe -web -listen "127.0.0.1:8080"
```

僅允許 loopback IP，不接受 `0.0.0.0` 或區網綁定；請使用程式印出的 IP 網址，其他 Host 名稱會被拒絕。`Ctrl+C` 會取消工作、關閉 WebSocket 並清理未完成 MP4。

| Web 參數 | 預設 | 用途 |
| --- | --- | --- |
| `-web` | false | 啟動 Web 模式 |
| `-listen` | `127.0.0.1:8080` | loopback IP 與連接埠 |
| `-output-root` | `downloads` | 伺服器本機成果根目錄 |
| `-profiles` | `config/site-profiles.json` | 可持久化的網站設定檔 |
| `-init-config` | false | 產生個人 `envfile` 後結束，不顯示金鑰 |

在「下載任務」選擇自動比對、指定網站或不使用設定，填入來源與輸出檔名即可加入佇列。一次執行一個任務，等待上限 20 筆，影片內並行數為 1–16。可查看影片與處理紀錄、取消工作、補 Header 重試失敗影片，或在完成後下載 MP4。切片處理達 100% 不代表轉封裝已完成，以「成功」及下載按鈕為準。

已結束的任務可按「刪除任務與檔案」，確認後移除任務紀錄及其整個輸出目錄，包含 MP4、TS 和暫存檔。若 MP4 已被搬移或刪除，會直接清理剩餘目錄，不會因找不到檔案而報錯；目錄已不存在、重複刪除也視為成功。搬到其他位置的檔案不受影響，其他分頁會透過 WebSocket 同步移除任務。執行中的任務需先取消，待結束後才能刪除。

切片先寫入暫存目錄再依序合併，避免將整部影片累積於記憶體；單片上限 64 MiB。每個任務有獨立目錄，同名任務不互相覆寫。轉封裝失敗會保留 TS，取消則依 `keepTs` 處理已合併的 TS；未完成 MP4 不會提供下載。

### 網站下載設定

在「網站設定」可新增、編輯、複製、停用、刪除設定，或從 MissAV／HL365 範本另存。這些範本依需求提供，未對真實網站做下載保證；網域、Header 與下載方式都可自行修改。

| 範本 | 輸入與下載方式 |
| --- | --- |
| MissAV | 輸入 `surrit.com` 的直接 M3U8 URL；套用 `Referer: https://missav.ai/` 與範本 User-Agent，不啟動 Chrome。輸入 MissAV 網頁會提示改提供 M3U8。 |
| HL365 | 輸入 `hl365.com` 網頁網址；先使用 Chrome 捕獲 M3U8，再下載。預設等待 6 秒，可自訂。 |

「網站網域」與「媒體／CDN 網域」分開設定，可處理播放頁和影片來源不同主機的情況。預設精確比對，可明確使用 `*.example.com` 包含子網域。多筆設定符合相同媒體主機時，必須手動選取。

設定支援下載方式、等待秒數、並行數、保留 TS、下載 Header、獨立的偵測 Header，以及 page／playlist 入口的 Query 預設值。Query 僅補缺少的 key，保留原網址既有值及編碼。偵測 Header 不繼承下載 Header；目前不支援向 Chrome 注入 Cookie，下載 Cookie 可正常設定。

每筆 Header 可限制目標主機與 port；留空表示僅傳給該階段最初入口主機。跨 CDN 請自行加入需要的精確目標，重新導向也會檢查。Cookie 始終限最初 M3U8 主機與 port，敏感 Header 不允許萬用字元目標。

本次任務可覆蓋 Header 與下載參數，也可明確清除值；模式由選定設定固定，要改模式需修改設定或選「不使用網站設定」。提交時凍結設定版本，編輯／刪除設定不影響已接受的工作。

設定重啟後保留；任務歷史只在本次服務期間保留，最多 100 筆，日誌每任務最多 500 筆。成果檔不自動刪除，重啟後不自動匯入任務列表。

Cookie、Authorization、自訂敏感 Header 及 Query 值預設不落盤。勾選保存後會以**本機明文**存入受帳號權限限制的設定檔；既有秘密不回顯，僅可保留、替換或清除。請勿將 `envfile`、`config/` 或成果檔加入版本控制。重試時可重新提供秘密，或明確套用目前已保存的設定版本。

完整 API、事件契約、執行計畫與驗收結果見 [docs/webfuncs.md](docs/webfuncs.md)。

## CLI 獨立執行與分主機部署

保留原有 CLI 參數，可將編譯後的執行檔單獨放在另一台主機執行：

```powershell
# 直接 M3U8，不啟動 Web 或 Chrome
.\m3u8-dl.exe -m3u8 "https://example.com/index.m3u8" -out "videos/movie.mp4" -referer "https://example.com/" -user-agent "Mozilla/5.0" -c 6

# 輸入網頁，由執行 CLI 的主機啟動 Chrome 偵測
.\m3u8-dl.exe -url "https://hl365.com/archives/222115.html" -mode page -out "videos/movie.mp4" -wait 10

# 從原始碼執行 CLI
go run . -m3u8 "https://example.com/index.m3u8" -out "movie.mp4"
```

CLI 主機需安裝 FFmpeg；網頁偵測模式另外需要 Chrome／Chromium。影片寫入 **CLI 執行主機**的 `-out` 路徑，相對路徑以目前工作目錄為準；來源標頭沿用 `-referer`、`-user-agent`、`-cookie`。CLI 失敗會回傳非零結束碼，可供批次或排程判斷。

例如主機 A 執行 CLI、主機 B 執行 `-web`，兩者可各自工作，不需要共享目錄、設定或資料庫；Web 的檔案刪除也只作用於 Web 主機的任務目錄。跨作業系統請為目標系統重新建置，不直接搬用 Windows `.exe`。

目前尚無「Web 遠端派工給另一台主機 CLI」的功能，Web 的 API／WebSocket 管理的是該 Web 程序的本機工作。未來遠端派工將以獨立 worker／傳輸介面擴充，保留既有 CLI 作為獨立下載工具；相關部署邊界見 [規劃文件](docs/webfuncs.md#14-cli-獨立執行與未來分主機部署)。

## 環境需求

- Go（目前 `go.mod` 宣告版本為 `1.27.2`）
- Google Chrome 或 Chromium（網頁偵測模式需要）
- FFmpeg，且 `ffmpeg` 指令可從 PATH 執行

## 第一次執行：準備步驟

以下以 Windows PowerShell 為例。

1. 安裝 Go、Google Chrome 或 Chromium，以及 FFmpeg。
2. 將 Go 和 FFmpeg 的執行檔目錄加入 `PATH`。例如 FFmpeg 位於 `D:\ffmpeg\ffmpeg.exe`，就將 `D:\ffmpeg` 加入 `PATH`，然後重新開啟 PowerShell。
3. 進入專案目錄，確認指令可用，並下載 Go 相依套件：

```powershell
cd D:\myprograms\m3u8downloader
go version
ffmpeg -version
go mod download
```

4. 執行下方的網頁下載或直接下載指令，將範例網址換成實際網址。網址與檔案路徑建議保留雙引號，避免網址中的 `&` 或路徑中的空白被 PowerShell 解讀。

## 下載網頁內的影片

```powershell
go run . -url "https://example.com/video-page" -out "videos/movie.mp4" -wait 10
```

預設在網頁載入後繼續監聽 6 秒；可用 `-wait` 延長。相同網址只處理一次，含不同查詢參數的網址視為不同項目。只有一筆時輸出 `movie.mp4`，多筆時依捕獲順序輸出 `movie_001.mp4`、`movie_002.mp4` 等，並自動建立輸出目錄。

執行流程如下：

1. 啟動瀏覽器並開啟指定網頁，監聽網路請求。
2. 在終端機列出捕獲的 M3U8 網址，持續等待指定秒數。
3. 結束偵測後，統計不重複的網址數量。
4. 逐一解析播放清單，下載與解密切片、合併 TS，再轉封裝為 MP4。
5. 顯示各影片的輸出位置，以及本次成功與失敗數量。

例如捕獲 3 個網址時，上述指令會輸出：

```text
videos/
  movie_001.mp4
  movie_002.mp4
  movie_003.mp4
```

若網頁載入較慢，可延長等待時間並調整切片下載數：

```powershell
go run . -url "https://example.com/video-page" -out "videos/movie.mp4" -wait 20 -c 10
```

偵測範圍是等待期間瀏覽器實際發出的、路徑含 `.m3u8` 的請求。需要手動點擊播放、切換集數或登入才會出現的影片，可能不會被捕獲。下載請求不會自動沿用瀏覽器的 Cookie 或標頭，可使用下方參數手動提供。

## 直接下載 M3U8

```powershell
go run . -m3u8 "https://example.com/index.m3u8?auth_key=..." -out "movie.mp4" -c 6
```

`-m3u8` 直接解析播放清單並下載，不啟動瀏覽器，也不需要安裝 Chrome。網址可以不含 `.m3u8` 副檔名；請保留完整驗證參數。不可與 `-url` 同時使用，也不可搭配 `-mode page`。

預設 `-mode auto` 會將路徑以 `.m3u8` 結尾的網址直接下載，其餘視為網頁。無副檔名的 M3U8 網址請加 `-mode m3u8`；也可以用 `-mode page` 強制執行網頁偵測。

```powershell
# 無 .m3u8 副檔名的播放清單
go run . -m3u8 "https://example.com/playlist?id=123" -out "movie.mp4"

# 強制將輸入當作網頁處理
go run . -url "https://example.com/video-page" -mode page -out "movie.mp4" -wait 10

# 轉封裝成功後仍保留 TS 檔案
go run . -url "https://example.com/index.m3u8" -out "movie.mp4" -keep-ts
```

Master playlist 沿用原有行為，下載第一個子播放清單；若瀏覽器也捕獲其他子播放清單，會分別處理，因此可能輸出同一影片的不同畫質或重複內容。此工具不會主動切換所有畫質或遍歷所有集數。

## 來源回傳 HTTP 403

可從瀏覽器開發者工具的 Network 面板，找到成功播放時的 M3U8 請求，將其 Referer、User-Agent、Cookie 填入下載參數。Referer 應使用實際播放頁網址：

```powershell
go run . -m3u8 "https://example.com/index.m3u8" -out "movie.mp4" -referer "https://example.com/watch/123" -user-agent "瀏覽器請求中的 User-Agent" -cookie "session=你的值; other=你的值"
```

只需提供來源需要的標頭。這些設定會用於下載播放清單、子播放清單、影片切片及 AES 金鑰；Cookie 僅傳送至最初 M3U8 網址的同一主機與連接埠，不會傳給其他來源。這些參數不影響網頁偵測時的瀏覽器。

HTTP 403 表示來源拒絕存取。若顯示 Cloudflare 拒絕存取，來源仍可能要求瀏覽器驗證或限制 IP；補上標頭不保證可下載。若瀏覽器也無法播放，請先確認來源網址與存取權限。

## 參數

| 參數 | 預設值 | 說明 |
| --- | --- | --- |
| `-url` | 空白 | 網頁或 M3U8 網址；與 `-m3u8` 擇一提供 |
| `-m3u8` | 空白 | 直接下載播放清單，跳過網頁解析；與 `-url`、`-mode page` 不可同時使用 |
| `-referer` | 空白 | 下載請求使用的播放頁 Referer |
| `-user-agent` | 空白 | 自訂下載請求的 User-Agent；未設定時沿用 Go HTTP 預設值 |
| `-cookie` | 空白 | 下載請求的 Cookie，僅傳送至最初播放清單的主機與連接埠 |
| `-out` | `output.mp4` | 輸出檔名，多筆自動加編號；副檔名統一為 `.mp4` |
| `-wait` | `6` | 網頁載入後繼續監聽的秒數，不可小於 0；直接下載模式不使用 |
| `-mode` | `auto` | `auto`、`page` 或 `m3u8` |
| `-c` | `6` | 每個影片的切片並行下載數，必須大於 0 |
| `-keep-ts` | `false` | 保留轉封裝前的 TS 檔案 |

影片逐一下載；單筆失敗會繼續處理下一筆，最後列出成功與失敗數。有任何失敗時以非零狀態碼結束。切片不完整時不輸出 MP4；FFmpeg 轉封裝失敗時保留 TS，供後續處理。輸出檔名已存在時會覆寫。

TS 暫存檔位於輸出目錄，名稱為 `temp_merged_隨機字串.ts`；轉封裝成功後預設刪除，加上 `-keep-ts` 才會保留。多筆下載失敗時不會重新編號，例如第 2 筆失敗，仍可能得到 `movie_001.mp4` 與 `movie_003.mp4`。

## 編譯與執行

可先編譯一次，之後直接執行，不必每次使用 `go run`：

```powershell
go build -o m3u8-dl.exe .
.\m3u8-dl.exe -url "https://example.com/video-page" -out "movie.mp4" -wait 10
```

查看完整參數說明：

```powershell
go run . -h
# 或使用已編譯的程式
.\m3u8-dl.exe -h
```

## 常見問題

| 狀況 | 處理方式 |
| --- | --- |
| 找不到 `go` 或 `ffmpeg` | 確認已安裝並加入 `PATH`，重新開啟 PowerShell，再執行版本檢查。 |
| 網頁偵測時無法啟動瀏覽器 | 確認已安裝 Chrome 或 Chromium，並可正常啟動。 |
| 等待期間未捕獲任何 M3U8 | 增加 `-wait`；確認頁面是否必須登入、點擊播放或切換集數才會載入影片。程式目前不會自動操作這些步驟。 |
| 已有播放清單網址，卻啟動網頁偵測 | 使用 `-m3u8 "網址"` 直接下載，或在 `-url` 後加上 `-mode m3u8`。 |
| 播放清單、切片或金鑰出現 HTTP 403 | 確認網址含完整且未過期的驗證參數；可用 `-referer`、`-user-agent`、`-cookie` 提供成功播放時的標頭。Cloudflare 或 IP 限制仍可能拒絕存取。 |
| 某個影片切片下載失敗 | 該影片會回報失敗，並繼續處理下一筆；確認來源可用後可用該 M3U8 網址單獨重試。 |
| FFmpeg 轉封裝失敗 | 查看終端機中的 FFmpeg 錯誤與保留的 TS 路徑；來源格式或音視訊編碼可能無法直接封裝進 MP4。 |
| 輸出多個內容相同的影片 | Master 與子播放清單可能對應相同內容；程式只去除完全相同的網址，不比對影片內容。 |

## 開發驗證

```powershell
go test ./...
go vet ./...
```

### Missav

```
go run . -m3u8 "https://surrit.com/8ef39691-6a8c-4c09-a28c-53460d3ff227/720p/video.m3u8" -out "out.mp4" -referer "https://missav.ai/" -user-agent "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
```

目前自動化測試涵蓋播放清單重新導向、相對路徑、驗證參數與金鑰解析、循環引用，以及切片缺失時不建立輸出檔案。
