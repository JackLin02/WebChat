package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Client struct {
	conn     *websocket.Conn
	username string
	send     chan []byte
}

type Message struct {
	Type     string `json:"type"`
	Username string `json:"username,omitempty"`
	Content  string `json:"content"`
	Target   string `json:"target,omitempty"`
	Avatar   string `json:"avatar,omitempty"`
}

var (
	clients    = make(map[*Client]bool)
	broadcast  = make(chan Message)
	register   = make(chan *Client)
	unregister = make(chan *Client)
	mu         sync.Mutex
)

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleConnections)

	go handleBroadcast()
	go handleClientRegistration()

	log.Println("服务器启动在：8080端口")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("升级websocket失败：", err)
		return
	}
	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
	}
	register <- client
	log.Println("客户端已经注册")

	go handleMessages(client)
	go handleClientSend(client)
}

func handleClientSend(client *Client) {
	defer func() {
		log.Printf("handleClientSend 退出 %s", client.username)
		client.conn.Close()
	}()

	for message := range client.send {
		log.Printf("准备发送消息给 %s: %s", client.username, string(message))
		w, err := client.conn.NextWriter(websocket.TextMessage)
		if err != nil {
			log.Printf("获取下一个Writer失败：%v", err)
			continue
		}
		if _, err := w.Write(message); err != nil {
			log.Printf("写入消息失败：%v", err)
			continue
		}
		if err := w.Close(); err != nil {
			log.Printf("关闭Writer失败：%v", err)
			continue
		}
	}
}

func handleMessages(client *Client) {
	defer func() {
		if client.username != "" {
			log.Printf("客户端断开连接：%s", client.username)
			broadcast <- Message{
				Type:    "system",
				Content: client.username + " 已离线",
			}
			// 更新用户列表
			sendUserListToAll()
		}
		unregister <- client
		client.conn.Close()
	}()

	for {
		var msg Message
		if err := client.conn.ReadJSON(&msg); err != nil {
			log.Printf("读取消息失败：%v", err)
			break
		}

		switch msg.Type {
		case "set_username":
			if isUsernameTaken(msg.Username) {
				jsonMsg, err := json.Marshal(Message{
					Type:    "system",
					Content: "用户名已占用",
				})
				if err != nil {
					log.Printf("序列化系统消息失败：%v", err)
					continue
				}
				client.send <- jsonMsg
				continue
			}
			client.username = msg.Username
			// 发送系统消息
			systemMsg := Message{
				Type:    "system",
				Content: msg.Username + " 已上线",
			}
			// 发送系统消息
			log.Printf("发送系统消息：%+v", systemMsg)
			broadcast <- systemMsg
			log.Println("准备发送用户列表消息")
			sendUserListToAll()
		case "message":
			if msg.Target != "" {
				log.Printf("私聊消息: %s -> %s", client.username, msg.Target)
				var targets []*Client

				mu.Lock()
				for c := range clients {
					if c.username == msg.Target || c.username == client.username {
						targets = append(targets, c)
					}
				}
				mu.Unlock()

				for _, c := range targets {
					privateMsg := Message{
						Type:     "private",
						Username: client.username,
						Content:  msg.Content,
						Target:   msg.Target,
					}
					jsonMsg, err := json.Marshal(privateMsg)
					if err != nil {
						log.Printf("序列化私聊消息失败：%v", err)
						continue
					}
					log.Printf("发送私聊消息：%s", string(jsonMsg))
					c.send <- jsonMsg
				}
			} else {
				mu.Unlock()
			}
		default:
			log.Printf("未知消息类型：%s", msg.Type)
		}
	}
}

func isUsernameTaken(username string) bool {
	mu.Lock()
	defer mu.Unlock()
	for client := range clients {
		if client.username == username {
			return true
		}
	}
	return false
}

func handleBroadcast() {
	for {
		msg := <-broadcast
		jsonMsg, err := json.Marshal(msg)
		if err != nil {
			log.Printf("error marshaling message: %v", err)
			continue
		}
		mu.Lock()
		for client := range clients {
			select {
			case client.send <- jsonMsg:
				log.Printf("消息发送给客户端：%s", client.username)
			default:
				close(client.send)
				delete(clients, client)
			}
		}
		mu.Unlock()
	}
}

func handleClientRegistration() {
	for {
		select {
		case client := <-register:
			log.Printf("注册新客户端：%p", client)
			mu.Lock()
			clients[client] = true
			mu.Unlock()
		case client := <-unregister:
			log.Printf("注销客户端：%p", client)
			mu.Lock()
			if _, ok := clients[client]; ok {
				delete(clients, client)
				client.conn.Close()
				mu.Unlock()
				//发送更新后的用户列表
				sendUserListToAll()
			} else {
				mu.Unlock()
			}
		}
	}
}

func sendUserListToAll() {
	mu.Lock()
	userList := make([]string, 0, len(clients))
	for client := range clients {
		userList = append(userList, client.username)
	}
	mu.Unlock()
	log.Printf("当前在线用户列表：%+v", userList)

	// 创建用户列表消息
	msg := Message{
		Type:    "user_list",
		Content: strings.Join(userList, ","),
	}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		log.Printf("用户列表序列化错误：%v", err)
		return
	}
	log.Printf("发送用户列表信息：%s", string(jsonMsg))

	mu.Lock()
	for client := range clients {
		select {
		case client.send <- jsonMsg:
			log.Printf("用户列表发送给客户端：%s", client.username)
		default:
			delete(clients, client)
		}
	}
	mu.Unlock()
}
