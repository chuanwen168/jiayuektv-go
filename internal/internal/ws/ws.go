// Package ws 实现 /ws WebSocket 服务：
//   - 连接建立后立即推送当前点歌队列；
//   - 队列变化时向所有客户端广播 {type:"queue", data:[...]}；
//   - 客户端发来的 control（控制端→播放端）、state / progress（播放端上报
//     状态）原样转发给所有客户端（含发送者，与原 Node.js 版行为一致）。
package ws

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"ktvhome/internal/logger"
)

// Upgrader 升级 HTTP 连接为 WebSocket。
var Upgrader = websocket.Upgrader{
	// 局域网应用，不做跨域来源校验。
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Hub 管理全部 WebSocket 连接并负责广播。
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}

	// GetQueue 返回当前队列 JSON 负载（由 main 注入，避免循环依赖）。
	GetQueue func() []byte
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*websocket.Conn]struct{})}
}

// Handle 处理一次 WebSocket 连接。
func (h *Hub) Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("WS", "WebSocket 升级失败: "+err.Error())
		return
	}

	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		_ = conn.Close()
	}()

	// 连接即推送当前队列。
	if h.GetQueue != nil {
		payload := h.GetQueue()
		if payload != nil {
			_ = conn.WriteMessage(websocket.TextMessage, payload)
		}
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// 解析失败则忽略；control/state/progress 消息广播给所有客户端。
		var p map[string]any
		if json.Unmarshal(msg, &p) != nil {
			continue
		}
		switch t, _ := p["type"].(string); t {
		case "control", "state", "progress":
			h.Broadcast(msg)
		}
	}
}

// Broadcast 向所有客户端发送消息。
func (h *Hub) Broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}
