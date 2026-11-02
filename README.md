# M3U8 網頁偵測與下載工具

`fetchz.go` 已合併至 `main.go`。程式可直接下載 M3U8，或透過 Chrome 監聽網頁請求，收集所有偵測到的 M3U8 網址後逐一下載、解密 AES-128 切片，並使用 FFmpeg 轉封裝成 MP4。

## 主要功能

* 輸入網頁網址，自動收集等待期間出現的多個 M3U8 網址並去除重複。
* 收集完成後逐一下載影片，每個影片內的切片可並行下載。
* 保留直接輸入 M3U8 網址的操作方式，也能手動指定輸入類型。
* 解析相對路徑、子播放清單與 AES-128 金鑰；資源網址未帶查詢參數時，沿用播放清單的參數。
* 依切片順序合併，以 FFmpeg 複製音視訊串流轉封裝為 MP4，不重新編碼。
* 多筆結果自動加上編號，並建立輸出目錄。
* 單筆下載失敗後繼續處理其他項目，最後顯示成功與失敗數量。

## 環境需求

* Go（目前 `go.mod` 宣告版本為 `1.27.2`）
* Google Chrome 或 Chromium（網頁偵測模式需要）
* FFmpeg，且 `ffmpeg` 指令可從 PATH 執行

## 第一次執行：準備步驟

以下以 Windows PowerShell 為例。

1. 安裝 Go、Google Chrome 或 Chromium，以及 FFmpeg。
2. 將 Go 和 FFmpeg 的執行檔目錄加入 `PATH`。例如 FFmpeg 位於 `D:\\ffmpeg\\ffmpeg.exe`，就將 `D:\\ffmpeg` 加入 `PATH`，然後重新開啟 PowerShell。
3. 進入專案目錄，確認指令可用，並下載 Go 相依套件：

```powershell
cd D:\\myprograms\\m3u8downloader
go version
ffmpeg -version
go mod download
```

4. 執行下方的網頁下載或直接下載指令，將範例網址換成實際網址。網址與檔案路徑建議保留雙引號，避免網址中的 `\&` 或路徑中的空白被 PowerShell 解讀。

## 下載網頁內的影片

```powershell
go run main.go -url "https://example.com/video-page" -out "videos/movie.mp4" -wait 10
```

預設在網頁載入後繼續監聽 6 秒；可用 `-wait` 延長。相同網址只處理一次，含不同查詢參數的網址視為不同項目。只有一筆時輸出 `movie.mp4`，多筆時依捕獲順序輸出 `movie\_001.mp4`、`movie\_002.mp4` 等，並自動建立輸出目錄。

執行流程如下：

1. 啟動瀏覽器並開啟指定網頁，監聽網路請求。
2. 在終端機列出捕獲的 M3U8 網址，持續等待指定秒數。
3. 結束偵測後，統計不重複的網址數量。
4. 逐一解析播放清單，下載與解密切片、合併 TS，再轉封裝為 MP4。
5. 顯示各影片的輸出位置，以及本次成功與失敗數量。

例如捕獲 3 個網址時，上述指令會輸出：

```text
videos/
  movie\_001.mp4
  movie\_002.mp4
  movie\_003.mp4
```

若網頁載入較慢，可延長等待時間並調整切片下載數：

```powershell
go run main.go -url "https://example.com/video-page" -out "videos/movie.mp4" -wait 20 -c 10
```

偵測範圍是等待期間瀏覽器實際發出的、路徑含 `.m3u8` 的請求。需要手動點擊播放、切換集數或登入才會出現的影片，可能不會被捕獲。下載請求不會自動沿用瀏覽器的 Cookie 或標頭，可使用下方參數手動提供。

## 直接下載 M3U8

```powershell
go run main.go -m3u8 "https://example.com/index.m3u8?auth\_key=..." -out "movie.mp4" -c 6
```

`-m3u8` 直接解析播放清單並下載，不啟動瀏覽器，也不需要安裝 Chrome。網址可以不含 `.m3u8` 副檔名；請保留完整驗證參數。不可與 `-url` 同時使用，也不可搭配 `-mode page`。

預設 `-mode auto` 會將路徑以 `.m3u8` 結尾的網址直接下載，其餘視為網頁。無副檔名的 M3U8 網址請加 `-mode m3u8`；也可以用 `-mode page` 強制執行網頁偵測。

```powershell
# 無 .m3u8 副檔名的播放清單
go run main.go -m3u8 "https://example.com/playlist?id=123" -out "movie.mp4"

# 強制將輸入當作網頁處理
go run main.go -url "https://example.com/video-page" -mode page -out "movie.mp4" -wait 10

# 轉封裝成功後仍保留 TS 檔案
go run main.go -url "https://example.com/index.m3u8" -out "movie.mp4" -keep-ts
```

Master playlist 沿用原有行為，下載第一個子播放清單；若瀏覽器也捕獲其他子播放清單，會分別處理，因此可能輸出同一影片的不同畫質或重複內容。此工具不會主動切換所有畫質或遍歷所有集數。

## 來源回傳 HTTP 403

可從瀏覽器開發者工具的 Network 面板，找到成功播放時的 M3U8 請求，將其 Referer、User-Agent、Cookie 填入下載參數。Referer 應使用實際播放頁網址：

```powershell
go run main.go -m3u8 "https://example.com/index.m3u8" -out "movie.mp4" -referer "https://example.com/watch/123" -user-agent "瀏覽器請求中的 User-Agent" -cookie "session=你的值; other=你的值"
```

只需提供來源需要的標頭。這些設定會用於下載播放清單、子播放清單、影片切片及 AES 金鑰；Cookie 僅傳送至最初 M3U8 網址的同一主機與連接埠，不會傳給其他來源。這些參數不影響網頁偵測時的瀏覽器。

HTTP 403 表示來源拒絕存取。若顯示 Cloudflare 拒絕存取，來源仍可能要求瀏覽器驗證或限制 IP；補上標頭不保證可下載。若瀏覽器也無法播放，請先確認來源網址與存取權限。

## 參數

|參數|預設值|說明|
|-|-|-|
|`-url`|空白|網頁或 M3U8 網址；與 `-m3u8` 擇一提供|
|`-m3u8`|空白|直接下載播放清單，跳過網頁解析；與 `-url`、`-mode page` 不可同時使用|
|`-referer`|空白|下載請求使用的播放頁 Referer|
|`-user-agent`|空白|自訂下載請求的 User-Agent；未設定時沿用 Go HTTP 預設值|
|`-cookie`|空白|下載請求的 Cookie，僅傳送至最初播放清單的主機與連接埠|
|`-out`|`output.mp4`|輸出檔名，多筆自動加編號；副檔名統一為 `.mp4`|
|`-wait`|`6`|網頁載入後繼續監聽的秒數，不可小於 0；直接下載模式不使用|
|`-mode`|`auto`|`auto`、`page` 或 `m3u8`|
|`-c`|`6`|每個影片的切片並行下載數，必須大於 0|
|`-keep-ts`|`false`|保留轉封裝前的 TS 檔案|

影片逐一下載；單筆失敗會繼續處理下一筆，最後列出成功與失敗數。有任何失敗時以非零狀態碼結束。切片不完整時不輸出 MP4；FFmpeg 轉封裝失敗時保留 TS，供後續處理。輸出檔名已存在時會覆寫。

TS 暫存檔位於輸出目錄，名稱為 `temp\_merged\_隨機字串.ts`；轉封裝成功後預設刪除，加上 `-keep-ts` 才會保留。多筆下載失敗時不會重新編號，例如第 2 筆失敗，仍可能得到 `movie\_001.mp4` 與 `movie\_003.mp4`。

## 編譯與執行

可先編譯一次，之後直接執行，不必每次使用 `go run`：

```powershell
go build -o m3u8-dl.exe .
.\\m3u8-dl.exe -url "https://example.com/video-page" -out "movie.mp4" -wait 10
```

查看完整參數說明：

```powershell
go run main.go -h
# 或使用已編譯的程式
.\\m3u8-dl.exe -h
```

## 常見問題

|狀況|處理方式|
|-|-|
|找不到 `go` 或 `ffmpeg`|確認已安裝並加入 `PATH`，重新開啟 PowerShell，再執行版本檢查。|
|網頁偵測時無法啟動瀏覽器|確認已安裝 Chrome 或 Chromium，並可正常啟動。|
|等待期間未捕獲任何 M3U8|增加 `-wait`；確認頁面是否必須登入、點擊播放或切換集數才會載入影片。程式目前不會自動操作這些步驟。|
|已有播放清單網址，卻啟動網頁偵測|使用 `-m3u8 "網址"` 直接下載，或在 `-url` 後加上 `-mode m3u8`。|
|播放清單、切片或金鑰出現 HTTP 403|確認網址含完整且未過期的驗證參數；可用 `-referer`、`-user-agent`、`-cookie` 提供成功播放時的標頭。Cloudflare 或 IP 限制仍可能拒絕存取。|
|某個影片切片下載失敗|該影片會回報失敗，並繼續處理下一筆；確認來源可用後可用該 M3U8 網址單獨重試。|
|FFmpeg 轉封裝失敗|查看終端機中的 FFmpeg 錯誤與保留的 TS 路徑；來源格式或音視訊編碼可能無法直接封裝進 MP4。|
|輸出多個內容相同的影片|Master 與子播放清單可能對應相同內容；程式只去除完全相同的網址，不比對影片內容。|

## 開發驗證

```powershell
go test ./...
go vet ./...
```

### Missav 下載範例

```
go run main.go -m3u8 "https://surrit.com/8....../video.m3u8" -out "out.mp4" -referer "https://missav.ai/" -user-agent "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
```

目前自動化測試涵蓋播放清單重新導向、相對路徑、驗證參數與金鑰解析、循環引用，以及切片缺失時不建立輸出檔案。

