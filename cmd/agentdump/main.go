// agentdump — 调试工具：拉取账号的 agent 列表与各 agent 详情的原始 JSON，
// 用于核对 InferHub 注册模型名（活动模型等）。仅供本地排查使用。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

func main() {
	path := flag.String("auth", "", "path to auth json file")
	flag.Parse()
	if *path == "" {
		log.Fatalf("usage: agentdump -auth <file.json>")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		log.Fatalf("read auth: %v", err)
	}
	var a auth.Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		log.Fatalf("parse auth: %v", err)
	}

	c := upstream.New(60 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	list, err := c.DebugGetSignedRaw(ctx, upstream.SnapEngineApiHost+upstream.EpAgentList+"?offset=0&limit=100", &a, true)
	if err != nil {
		log.Fatalf("agent list: %v", err)
	}
	fmt.Println("=== AGENTS RAW ===")
	fmt.Println(string(list))

	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(list, &out); err != nil {
		log.Fatalf("parse agents: %v", err)
	}
	for _, ag := range out.Agents {
		fmt.Printf("\n=== DETAIL agent=%s name=%s primary=%v ===\n", ag.AgentID, ag.AgentName, ag.Primary)
		d, err := c.DebugGetSignedRaw(ctx, upstream.SnapEngineApiHost+upstream.EpAgentDetail+"?agent_id="+url.QueryEscape(ag.AgentID), &a, true)
		if err != nil {
			fmt.Println("ERR:", err)
			continue
		}
		fmt.Println(string(d))
	}
}
