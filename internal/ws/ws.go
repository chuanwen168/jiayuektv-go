// Package ws 实现 /ws WebSocket 服务：
//   - 连接建立后立即推送当前点歌队列；
//   - 队列变化时向所有客户端广播 {type:"queue", data:[...]}；
//   - 客户端发来的 control（控制端→播放端）、state / progress（播放端上报
//     状态）原样转发给所有客户端（含发送者，与原 Node.js 版行为一致）。
//
// 1.0.4 修复慢客户端阻塞：每个连接拥有独立的缓冲发送队列与写 goroutine，
// 广播时只做非阻塞投递（队列满即关闭慢连接），避免一个弱网设备卡住写缓冲
// 导致全局广播（head-of-line blocking）；同时增加 ping/pong 心跳，掉线连接
// 按读超时自动回收，不再长期泄漏。
package ws

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ktvhome/internal/logger"
)

// Upgrader 升级 HTTP 连接为 WebSocket。
var Upgrader = websocket.Upgrader{
	// 局域网应用，不做跨域来源校验。
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	// sendBufSize 每连接发送队列容量（条）。
	sendBufSize = 64
	// pingInterval 心跳间隔。
	pingInterval = 30 * time.Second
	// pongWait 等待 pong 的最长时间（读超时）。
	pongWait = 60 * time.Second
)

// client 封装一条 WebSocket 连接：send 为独立缓冲发送队列，
// 由专属写 goroutine 消费，写操作不再持 Hub 全局锁。
type client struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub 管理全部 WebSocket 连接并负责广播。
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}

	// GetQueue 返回当前队列 JSON 负载（由 main 注入，避免循环依赖）。
	GetQueue func() []byte
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

// Handle 处理一次 WebSocket 连接。
func (h *Hub) Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("WS", "WebSocket 升级失败: "+err.Error())
		return
	}
	c := &client{conn: conn, send: make(chan []byte, sendBufSize)}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		close(c.send)
		_ = conn.Close()
	}()

	// 连接即推送当前队列（直写，避免经队列排队被心跳挤掉）。
	if h.GetQueue != nil {
		if payload := h.GetQueue(); payload != nil {
			_ = conn.WriteMessage(websocket.TextMessage, payload)
		}
	}

	// 专属写 goroutine：消费 send 队列 + 定期心跳 ping。
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case msg, ok := <-c.send:
				if !ok {
					return
				}
				if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
			case <-ticker.C:
				if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	// 读循环：pong 重置读超时；control/state/progress 广播给所有客户端。
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var p map[string]any
		if json.Unmarshal(msg, &p) != nil {
			continue
		}
		switch t, _ := p["type"].(string); t {
		case "control", "state", "progress", "effect":
			h.Broadcast(msg)
		}
	}
}

// Broadcast 向所有客户端非阻塞投递消息：队列满说明该连接消费不及
// （弱网/掉线），直接断开它——conn.Close 会让读循环立即报错退出，
// 由 Handle 的 defer 统一清理（delete + close(send)），绝不让慢客户端
// 拖住全局广播，也避免两处重复 close 同一 channel。
func (h *Hub) Broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			logger.Warn("WS", "客户端发送队列已满（慢客户端），断开连接以保护全局广播")
			_ = c.conn.Close()
			delete(h.clients, c)
		}
	}
}
