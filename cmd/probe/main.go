package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

// redTestPNG 64x64 纯红 PNG（media 模式测试图，程序内生成零外部依赖）。
func redTestPNG() []byte {
	const w, h = 64, 64
	raw := make([]byte, 0, h*(1+w*3))
	for y := 0; y < h; y++ {
		raw = append(raw, 0)
		for x := 0; x < w; x++ {
			raw = append(raw, 0xff, 0x00, 0x00)
		}
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	chunk := func(typ string, data []byte) []byte {
		c := append(append([]byte{}, typ...), data...)
		out := make([]byte, 0, len(c)+8)
		out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
		out = append(out, c...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(c))
	}
	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	png := []byte("\x89PNG\r\n\x1a\n")
	png = append(png, chunk("IHDR", ihdr)...)
	png = append(png, chunk("IDAT", zbuf.Bytes())...)
	png = append(png, chunk("IEND", nil)...)
	return png
}

func main() {
	auths, err := auth.LoadDir("./auths")
	if err != nil || len(auths) == 0 {
		panic(fmt.Sprintf("auths: %v len=%d", err, len(auths)))
	}
	a := auths[0]
	cred := upstream.SignCredential{
		AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok,
	}
	c := upstream.New(60 * time.Second)
	msg := "hi"
	if len(os.Args) > 1 {
		msg = os.Args[1]
	}
	mode := "chatstream"
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	model := "GLM-5.2"
	if len(os.Args) > 3 {
		model = os.Args[3]
	}
	// growth 模式（SPEC §32）：直查腾讯成长中心端点，打印原始响应（形状实证）。
	if mode == "growth" {
		// 账号选择：OMNIGATE_PROBE_UID 指定 uid（CN/全球双账号并存时用），
		// 缺省取最后一个 workbuddy 账号。
		wantUID := os.Getenv("OMNIGATE_PROBE_UID")
		var wb *auth.Auth
		for _, x := range auths {
			if x.Profile != "workbuddy" {
				continue
			}
			if wantUID != "" && x.UserID != wantUID {
				continue
			}
			wb = x
		}
		if wb == nil {
			panic("no workbuddy auth found (OMNIGATE_PROBE_UID=" + wantUID + ")")
		}
		fmt.Println("account uid=", wb.UserID, "domain=", wb.Domain)
		path := "/activity/growth/tasks"
		if msg != "" {
			path = msg
		}
		tc := upstream.NewTencent(30 * time.Second)
		if mode2 := os.Getenv("OMNIGATE_PROBE_METHOD"); mode2 == "POST" {
			raw, st, err := tc.DebugPostBody(wb, path, os.Getenv("OMNIGATE_PROBE_BODY"))
			fmt.Println("POST", path, "http=", st, "err=", err)
			fmt.Println(string(raw)) // 完整响应（截断交给调用方按需处理）
			return
		}
		raw, st, err := tc.DebugGet(wb, path)
		fmt.Println("GET", path, "http=", st, "err=", err)
		fmt.Println(string(raw))
		return
	}

	chatID := fmt.Sprintf("%032x", time.Now().UnixNano())[:32]
	fmt.Println("chat_id=", chatID, "user=", a.UserName, "uid=", a.UserID, "msg=", msg, "mode=", mode)

	var rc io.ReadCloser
	switch mode {
	case "tools":
		// SPEC §31 前置实证（2026-09-06 授权活测序列）：华为 MaaS 原生 tools 行为。
		// msg=toolschoice 时带 tool_choice:"auto"（网关 codearts 路径不带）——
		// 区分截断触发条件；观察 delta 是否出现原生 tool_calls。
		body := map[string]any{
			"model":  upstream.CanonicalModel(model),
			"stream": true,
			"messages": []any{map[string]any{
				"role":    "user",
				"content": "现在几点了？必须使用工具回答。",
			}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_current_time",
					"description": "获取当前时间",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}},
		}
		if msg == "toolschoice" {
			body["tool_choice"] = "auto"
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken)
	case "media":
		// SPEC §30.9 授权活测（2026-09-06 用户拍板，单帧最小化）：华为 MaaS 端点
		// 是否接受 OpenAI image_url 分片（data URI 内联）。msg 参数可传图片文件
		// 路径（转 data URI）或留空用内置 64x64 纯红 PNG。
		uri := msg
		if uri == "" {
			uri = "data:image/png;base64," + base64.StdEncoding.EncodeToString(redTestPNG())
		} else if !strings.Contains(uri, "data:") {
			raw, rerr := os.ReadFile(uri)
			if rerr != nil {
				panic(rerr)
			}
			uri = "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
		}
		body := map[string]any{
			"model":  upstream.CanonicalModel(model),
			"stream": true,
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "这张图片的主要颜色是什么？用一个词回答。"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}},
				},
			}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken)
	case "raw-role":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken)
	case "raw-text":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken)
	case "raw-blocks":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken)
	default:
		rc, err = c.ChatStream(context.Background(), chatID, []upstream.ChatMessage{{Role: "user", Content: msg}}, "", cred, a.UserName, model, nil, "", nil)
	}
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	// rawdump：上游响应**原样**转储（SPEC §33.1 检查单第 1 步"抓真实响应、列全部键路径"）。
	// 不做任何解析——派生的解析结果只会让我们看到"自己以为的形状"。
	if mode == "rawdump" {
		if _, err := io.Copy(os.Stdout, rc); err != nil {
			panic(err)
		}
		return
	}
	br := bufio.NewReaderSize(rc, 64*1024)
	n := 0
	var lastText string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			n++
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if strings.Contains(payload, `"text"`) {
					// extract text roughly
					if i := strings.Index(payload, `"text":"`); i >= 0 {
						rest := payload[i+8:]
						// naive until next unescaped "
						var b strings.Builder
						for j := 0; j < len(rest); j++ {
							if rest[j] == '\\' && j+1 < len(rest) {
								b.WriteByte(rest[j])
								b.WriteByte(rest[j+1])
								j++
								continue
							}
							if rest[j] == '"' {
								break
							}
							b.WriteByte(rest[j])
						}
						lastText = b.String()
					}
				}
			}
			if n <= 3 || strings.Contains(line, "DONE") || strings.Contains(line, "error_code") || strings.Contains(line, "error_msg") {
				if len(line) > 300 {
					fmt.Println(line[:300], "...")
				} else {
					fmt.Println(line)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
	}
	fmt.Println("total_lines=", n)
	if len(lastText) > 200 {
		fmt.Println("final_text=", lastText[:200], "...")
	} else {
		fmt.Println("final_text=", lastText)
	}
}
