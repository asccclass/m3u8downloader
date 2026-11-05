package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func initializeConfig(path string) error {
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return fmt.Errorf("無法建立設定（既有檔案不覆寫）")
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if e = secureFile(path); e != nil {
		return fmt.Errorf("無法限制設定檔權限")
	}
	_, e = fmt.Fprintf(f, "SystemName=M3U8Downloader\nSessionCookieAuthKey=%s\nTLSAutoEnabled=false\n", base64.StdEncoding.EncodeToString(key))
	if e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	success = true
	fmt.Println("已建立 envfile，可執行 go run . -web")
	return nil
}
