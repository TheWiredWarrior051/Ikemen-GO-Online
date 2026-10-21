package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type RoomInfo struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	HostName           string    `json:"hostName"`
	GuestName          string    `json:"guestName,omitempty"`
	HasPassword        bool      `json:"hasPassword"`
	Status             string    `json:"status"` // "waiting", "handshaking", "in_game"
	ContentFingerprint string    `json:"contentFingerprint"`
	PlayerCount        int       `json:"playerCount"`
	MaxPlayers         int       `json:"maxPlayers"`
	CreatedAt          time.Time `json:"createdAt"`
}

type WSMessage struct {
	Type               string            `json:"type"`
	RoomID             string            `json:"roomId,omitempty"`
	RoomName           string            `json:"roomName,omitempty"`
	PlayerName         string            `json:"playerName,omitempty"`
	Password           string            `json:"password,omitempty"`
	ContentFingerprint string            `json:"contentFingerprint,omitempty"`
	Room               *RoomInfo         `json:"room,omitempty"`
	Rooms              []RoomInfo        `json:"rooms,omitempty"`
	PeerName           string            `json:"peerName,omitempty"`
	ICEServers         []ICEServerConfig `json:"iceServers,omitempty"`
	Payload            json.RawMessage   `json:"payload,omitempty"`
	Error              string            `json:"error,omitempty"`
}

type Client struct {
	conn   *websocket.Conn
	sendMu sync.Mutex
	roomID string
	isHost bool
	name   string
}

func (c *Client) sendJSON(msg WSMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

func (c *Client) sendBinary(data []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

type Room struct {
	info     RoomInfo
	password string
	host     *Client
	guest    *Client
}

type LobbyServer struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func NewLobbyServer() *LobbyServer {
	return &LobbyServer{
		rooms: make(map[string]*Room),
	}
}

func generateRoomID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// Fetch TURN credentials from Cloudflare Realtime TURN API (if CF_TURN_TOKEN_ID & CF_TURN_API_TOKEN are set)
// or static TURN_SERVER / TURN_USER / TURN_PASS env vars.
func getServerICEServers() []ICEServerConfig {
	var out []ICEServerConfig

	cfTokenID := strings.TrimSpace(os.Getenv("CF_TURN_TOKEN_ID"))
	cfAPIToken := strings.TrimSpace(os.Getenv("CF_TURN_API_TOKEN"))
	if cfTokenID != "" && cfAPIToken != "" {
		url := fmt.Sprintf("https://rtc.live.cloudflare.com/v1/turn/keys/%s/credentials/generate", cfTokenID)
		req, err := http.NewRequest("POST", url, bytes.NewBufferString(`{"ttl": 86400}`))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+cfAPIToken)
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 3 * time.Second}
			if resp, err := client.Do(req); err == nil {
				defer resp.Body.Close()
				var parsed struct {
					ICEServers ICEServerConfig `json:"iceServers"`
				}
				if json.NewDecoder(resp.Body).Decode(&parsed) == nil && len(parsed.ICEServers.URLs) > 0 {
					out = append(out, parsed.ICEServers)
				}
			}
		}
	}

	if turnURL := strings.TrimSpace(os.Getenv("TURN_SERVER")); turnURL != "" {
		out = append(out, ICEServerConfig{
			URLs:       strings.Split(turnURL, ","),
			Username:   strings.TrimSpace(os.Getenv("TURN_USER")),
			Credential: strings.TrimSpace(os.Getenv("TURN_PASS")),
		})
	}
	return out
}

func (ls *LobbyServer) listRooms() []RoomInfo {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	out := make([]RoomInfo, 0, len(ls.rooms))
	for _, r := range ls.rooms {
		out = append(out, r.info)
	}
	return out
}

func (ls *LobbyServer) getPeer(c *Client) *Client {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	if c.roomID == "" {
		return nil
	}
	room, ok := ls.rooms[c.roomID]
	if !ok {
		return nil
	}
	if c.isHost {
		return room.guest
	}
	return room.host
}

func (ls *LobbyServer) removeClientFromRoom(c *Client) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if c.roomID == "" {
		return
	}
	r, ok := ls.rooms[c.roomID]
	c.roomID = ""
	if !ok {
		return
	}

	if r.host == c {
		guest := r.guest
		delete(ls.rooms, r.info.ID)
		log.Printf("[Lobby] Room %s (%q) closed (host %q disconnected/left)", r.info.ID, r.info.Name, c.name)
		if guest != nil {
			guest.roomID = ""
			_ = guest.sendJSON(WSMessage{
				Type:  "peer_left",
				Error: "Host closed the lobby",
			})
		}
	} else if r.guest == c {
		r.guest = nil
		r.info.GuestName = ""
		r.info.PlayerCount = 1
		r.info.Status = "waiting"
		log.Printf("[Lobby] Guest %q left room %s; room returned to waiting", c.name, r.info.ID)
		if r.host != nil {
			_ = r.host.sendJSON(WSMessage{
				Type:   "peer_left",
				RoomID: r.info.ID,
				Room:   &r.info,
				Error:  "Challenger left the lobby",
			})
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  32768,
	WriteBufferSize: 32768,
}

func (ls *LobbyServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Lobby] Upgrade error: %v", err)
		return
	}
	client := &Client{conn: conn}
	defer func() {
		ls.removeClientFromRoom(client)
		conn.Close()
	}()

	for {
		msgType, rawData, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Fast-path binary WebSocket relay for strict Symmetric NAT fallback (0x01 = sync TCP, 0x02 = GGPO UDP)
		if msgType == websocket.BinaryMessage {
			if peer := ls.getPeer(client); peer != nil {
				_ = peer.sendBinary(rawData)
			}
			continue
		}

		var msg WSMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "ping":
			_ = client.sendJSON(WSMessage{Type: "pong"})

		case "list_rooms":
			rooms := ls.listRooms()
			_ = client.sendJSON(WSMessage{
				Type:  "room_list",
				Rooms: rooms,
			})

		case "create_room":
			ls.removeClientFromRoom(client)
			ls.mu.Lock()
			id := generateRoomID()
			for ls.rooms[id] != nil {
				id = generateRoomID()
			}
			roomName := strings.TrimSpace(msg.RoomName)
			if roomName == "" {
				roomName = "Ikemen Arena #" + id
			}
			playerName := strings.TrimSpace(msg.PlayerName)
			if playerName == "" {
				playerName = "Player 1"
			}
			client.name = playerName
			client.roomID = id
			client.isHost = true

			info := RoomInfo{
				ID:                 id,
				Name:               roomName,
				HostName:           playerName,
				HasPassword:        msg.Password != "",
				Status:             "waiting",
				ContentFingerprint: msg.ContentFingerprint,
				PlayerCount:        1,
				MaxPlayers:         2,
				CreatedAt:          time.Now().UTC(),
			}
			ls.rooms[id] = &Room{
				info:     info,
				password: msg.Password,
				host:     client,
			}
			ls.mu.Unlock()

			iceServers := getServerICEServers()
			log.Printf("[Lobby] Room %s (%q) created by %q (fingerprint=%s)", id, roomName, playerName, msg.ContentFingerprint)
			_ = client.sendJSON(WSMessage{
				Type:       "room_created",
				RoomID:     id,
				Room:       &info,
				ICEServers: iceServers,
			})

		case "join_room":
			ls.removeClientFromRoom(client)
			ls.mu.Lock()
			roomID := strings.ToUpper(strings.TrimSpace(msg.RoomID))
			room, ok := ls.rooms[roomID]
			if !ok {
				ls.mu.Unlock()
				_ = client.sendJSON(WSMessage{
					Type:  "error",
					Error: "Lobby room not found: " + roomID,
				})
				continue
			}
			if room.guest != nil || room.info.Status != "waiting" {
				ls.mu.Unlock()
				_ = client.sendJSON(WSMessage{
					Type:  "error",
					Error: "Lobby room is already full or in a match",
				})
				continue
			}
			if room.password != "" && room.password != msg.Password {
				ls.mu.Unlock()
				_ = client.sendJSON(WSMessage{
					Type:  "error",
					Error: "Incorrect lobby password",
				})
				continue
			}
			if room.info.ContentFingerprint != "" && msg.ContentFingerprint != "" && room.info.ContentFingerprint != msg.ContentFingerprint {
				ls.mu.Unlock()
				_ = client.sendJSON(WSMessage{
					Type:  "error",
					Error: "Game content/roster mismatch with host (" + room.info.ContentFingerprint[:8] + " vs " + msg.ContentFingerprint[:8] + ")",
				})
				continue
			}

			playerName := strings.TrimSpace(msg.PlayerName)
			if playerName == "" {
				playerName = "Challenger"
			}
			client.name = playerName
			client.roomID = roomID
			client.isHost = false

			room.guest = client
			room.info.GuestName = playerName
			room.info.PlayerCount = 2
			room.info.Status = "handshaking"
			infoCopy := room.info
			hostClient := room.host
			ls.mu.Unlock()

			iceServers := getServerICEServers()
			log.Printf("[Lobby] Guest %q joined room %s (%q)", playerName, roomID, infoCopy.Name)
			_ = client.sendJSON(WSMessage{
				Type:       "room_joined",
				RoomID:     roomID,
				Room:       &infoCopy,
				PeerName:   infoCopy.HostName,
				ICEServers: iceServers,
			})
			if hostClient != nil {
				_ = hostClient.sendJSON(WSMessage{
					Type:       "peer_joined",
					RoomID:     roomID,
					Room:       &infoCopy,
					PeerName:   playerName,
					ICEServers: iceServers,
				})
			}

		case "signal", "relay_fallback":
			if target := ls.getPeer(client); target != nil {
				_ = target.sendJSON(WSMessage{
					Type:    msg.Type,
					RoomID:  client.roomID,
					Payload: msg.Payload,
				})
			}

		case "match_started":
			ls.mu.Lock()
			if room, ok := ls.rooms[client.roomID]; ok && client.isHost {
				room.info.Status = "in_game"
			}
			ls.mu.Unlock()

		case "leave_room":
			ls.removeClientFromRoom(client)
			_ = client.sendJSON(WSMessage{Type: "left_room"})
		}
	}
}

func (ls *LobbyServer) handleRoomsREST(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	rooms := ls.listRooms()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"rooms": rooms,
	})
}

func main() {
	defaultAddr := ":8080"
	if envPort := strings.TrimSpace(os.Getenv("PORT")); envPort != "" {
		defaultAddr = ":" + envPort
	}
	addr := flag.String("addr", defaultAddr, "HTTP/WebSocket listen address")
	flag.Parse()

	server := NewLobbyServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"ikemen-go-lobby-server","status":"ok"}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/rooms", server.handleRoomsREST)
	mux.HandleFunc("/ws", server.handleWS)

	log.Printf("Ikemen GO Lobby & Signaling Server listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("Lobby server failed: %v", err)
	}
}
