package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type LobbyICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type LobbyRoomInfo struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	HostName           string    `json:"hostName"`
	GuestName          string    `json:"guestName,omitempty"`
	HasPassword        bool      `json:"hasPassword"`
	Status             string    `json:"status"`
	ContentFingerprint string    `json:"contentFingerprint"`
	PlayerCount        int       `json:"playerCount"`
	MaxPlayers         int       `json:"maxPlayers"`
	CreatedAt          time.Time `json:"createdAt"`
}

type lobbyWSMessage struct {
	Type               string           `json:"type"`
	RoomID             string           `json:"roomId,omitempty"`
	RoomName           string           `json:"roomName,omitempty"`
	PlayerName         string           `json:"playerName,omitempty"`
	Password           string           `json:"password,omitempty"`
	ContentFingerprint string           `json:"contentFingerprint,omitempty"`
	Room               *LobbyRoomInfo   `json:"room,omitempty"`
	Rooms              []LobbyRoomInfo  `json:"rooms,omitempty"`
	PeerName           string           `json:"peerName,omitempty"`
	ICEServers         []LobbyICEServer `json:"iceServers,omitempty"`
	Payload            json.RawMessage  `json:"payload,omitempty"`
	Error              string           `json:"error,omitempty"`
}

type lobbySignalPayload struct {
	SDP       *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}

const (
	wsRelayChannelSync byte = 0x01
	wsRelayChannelGGPO byte = 0x02
)

type LobbyManager struct {
	mu                sync.Mutex
	wsConn            *websocket.Conn
	wsSendMu          sync.Mutex
	pc                *webrtc.PeerConnection
	syncDC            *webrtc.DataChannel
	ggpoDC            *webrtc.DataChannel
	syncDCOpen        bool
	ggpoDCOpen        bool
	relayMode         bool
	bridgeStarted     bool
	remoteDescSet     bool
	pendingCandidates []webrtc.ICECandidateInit
	serverICEServers  []LobbyICEServer
	handshakeID       uint64

	syncMu        sync.Mutex
	syncPreBuf    [][]byte
	tcpBridgeConn *net.TCPConn

	udpMu         sync.RWMutex
	udpBridgeConn *net.UDPConn
	ggpoTarget    *net.UDPAddr
	localGGPOPort int
	udpProxyPort  int

	state     string // "idle", "connecting", "waiting", "handshaking", "connected", "error"
	roomID    string
	roomName  string
	peerName  string
	isHost    bool
	lastError string
	rooms     []LobbyRoomInfo
}

var (
	embeddedLobbyMu      sync.Mutex
	embeddedLobbyRunning bool
)

func NewLobbyManager() *LobbyManager {
	return &LobbyManager{
		state: "idle",
	}
}

func lobbyLogf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	if f, err := os.OpenFile("save/logs/lobby_client.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		_, _ = fmt.Fprintf(f, "%s [PID %d] %s\n", time.Now().Format("15:04:05.000"), os.Getpid(), msg)
		_ = f.Close()
	}
}

func (s *System) ensureLobby() *LobbyManager {
	if s.lobby == nil {
		s.lobby = NewLobbyManager()
	}
	return s.lobby
}

func (s *System) computeContentFingerprint() string {
	if s.contentFingerprint != "" {
		return s.contentFingerprint
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "sync_v=%d\n", syncConfigVersion)

	if s.motif.Files.Select != "" {
		if fp := FileExist(s.motif.Files.Select); fp != "" {
			if txt, err := LoadText(fp); err == nil {
				normalized := strings.ReplaceAll(txt, "\r\n", "\n")
				_, _ = h.Write([]byte("select.def:\n"))
				_, _ = h.Write([]byte(normalized))
			}
		}
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 16 {
		sum = sum[:16]
	}
	s.contentFingerprint = sum
	return s.contentFingerprint
}

func (lm *LobbyManager) wsURL() string {
	u := strings.TrimSpace(sys.cfg.Netplay.LobbyServer)
	if u == "" {
		u = "ws://127.0.0.1:8080/ws"
	}
	return u
}

func (lm *LobbyManager) httpBaseURL() string {
	u := lm.wsURL()
	u = strings.TrimSuffix(u, "/ws")
	u = strings.TrimSuffix(u, "/")
	if strings.HasPrefix(u, "wss://") {
		u = "https://" + strings.TrimPrefix(u, "wss://")
	} else if strings.HasPrefix(u, "ws://") {
		u = "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}

func (lm *LobbyManager) httpRoomsURL() string {
	return lm.httpBaseURL() + "/api/rooms"
}

func (lm *LobbyManager) httpHealthURL() string {
	return lm.httpBaseURL() + "/health"
}

func (lm *LobbyManager) isLocalServer() (bool, string) {
	parsed, err := url.Parse(lm.wsURL())
	if err != nil {
		return false, ""
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "wss" || parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "8080"
		}
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return true, "127.0.0.1:" + port
	}
	return false, ""
}

// EnsureServerReady automatically starts an in-process Lobby Server when LobbyServer is localhost
// and no server is running yet, or sends an async wake-up ping when LobbyServer is a remote Render URL.
func (lm *LobbyManager) EnsureServerReady() {
	isLocal, listenAddr := lm.isLocalServer()
	healthURL := lm.httpHealthURL()

	if isLocal {
		client := &http.Client{Timeout: 250 * time.Millisecond}
		if resp, err := client.Get(healthURL); err == nil {
			_ = resp.Body.Close()
			return
		}
		startEmbeddedLobbyServer(listenAddr)
		return
	}

	// Remote server (e.g. Render Free Tier): send an async wake-up request so cold starts happen in the background
	SafeGo(func() {
		client := &http.Client{Timeout: 30 * time.Second}
		if resp, err := client.Get(healthURL); err == nil {
			_ = resp.Body.Close()
			lobbyLogf("[Lobby] Remote lobby server awake: %s", healthURL)
		}
	})
}

func (lm *LobbyManager) FetchRooms() ([]LobbyRoomInfo, error) {
	lm.EnsureServerReady()
	isLocal, _ := lm.isLocalServer()
	timeout := 12 * time.Second
	if isLocal {
		timeout = 3 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(lm.httpRoomsURL())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var wrapper struct {
		Rooms []LobbyRoomInfo `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, err
	}

	lm.mu.Lock()
	lm.rooms = wrapper.Rooms
	lm.mu.Unlock()
	return wrapper.Rooms, nil
}

func (lm *LobbyManager) sendWS(msg lobbyWSMessage) error {
	lm.mu.Lock()
	conn := lm.wsConn
	lm.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected to lobby server")
	}
	lm.wsSendMu.Lock()
	defer lm.wsSendMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(8 * time.Second))
	return conn.WriteJSON(msg)
}

func (lm *LobbyManager) sendWSBinary(prefix byte, payload []byte) error {
	lm.mu.Lock()
	conn := lm.wsConn
	lm.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected to lobby server")
	}
	frame := make([]byte, 1+len(payload))
	frame[0] = prefix
	copy(frame[1:], payload)

	lm.wsSendMu.Lock()
	defer lm.wsSendMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (lm *LobbyManager) Reset() {
	lm.mu.Lock()
	lm.handshakeID++
	ws := lm.wsConn
	lm.wsConn = nil
	pc := lm.pc
	lm.pc = nil
	lm.syncDC = nil
	lm.ggpoDC = nil
	lm.syncDCOpen = false
	lm.ggpoDCOpen = false
	lm.relayMode = false
	lm.bridgeStarted = false
	lm.remoteDescSet = false
	lm.pendingCandidates = nil
	lm.serverICEServers = nil
	lm.state = "idle"
	lm.roomID = ""
	lm.roomName = ""
	lm.peerName = ""
	lm.lastError = ""
	lm.mu.Unlock()

	lm.syncMu.Lock()
	tcpC := lm.tcpBridgeConn
	lm.tcpBridgeConn = nil
	lm.syncPreBuf = nil
	lm.syncMu.Unlock()

	lm.udpMu.Lock()
	udpC := lm.udpBridgeConn
	lm.udpBridgeConn = nil
	lm.ggpoTarget = nil
	lm.udpMu.Unlock()

	if tcpC != nil {
		_ = tcpC.Close()
	}
	if udpC != nil {
		_ = udpC.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
	if ws != nil {
		_ = ws.Close()
	}
}

func (lm *LobbyManager) fail(msg string) {
	lobbyLogf("[Lobby] Error: %s", msg)
	lm.mu.Lock()
	lm.state = "error"
	lm.lastError = msg
	lm.mu.Unlock()
}

func (lm *LobbyManager) prepareEngineNetConnection() {
	if sys.netConnection != nil {
		sys.netConnection.end()
		sys.netConnection = nil
	}
	sys.sessionWarning = ""
	sys.chars = [len(sys.chars)][]*Char{}
	sys.netConnection = NewNetConnection()
	if sys.cfg.Netplay.RollbackNetcode {
		rs := NewRollbackSession(sys.cfg.Netplay.Rollback)
		sys.rollback.session = &rs
	}
}

func (lm *LobbyManager) dialLobbyWS() (*websocket.Conn, error) {
	lm.EnsureServerReady()
	isLocal, _ := lm.isLocalServer()
	timeout := 25 * time.Second
	if isLocal {
		timeout = 5 * time.Second
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		ReadBufferSize:   32768,
		WriteBufferSize:  32768,
	}
	conn, _, err := dialer.Dial(lm.wsURL(), nil)
	return conn, err
}

func (lm *LobbyManager) CreateRoom(roomName, playerName, password string) error {
	lm.Reset()
	lm.prepareEngineNetConnection()

	if strings.TrimSpace(playerName) == "" {
		playerName = sys.cfg.Netplay.PlayerName
	}
	fingerprint := sys.currentContentFingerprint()

	lm.mu.Lock()
	lm.state = "connecting"
	lm.isHost = true
	lm.roomName = roomName
	lm.mu.Unlock()

	SafeGo(func() {
		conn, err := lm.dialLobbyWS()
		if err != nil {
			lm.fail("Could not connect to lobby server: " + err.Error())
			return
		}
		lm.mu.Lock()
		lm.wsConn = conn
		lm.mu.Unlock()

		err = lm.sendWS(lobbyWSMessage{
			Type:               "create_room",
			RoomName:           roomName,
			PlayerName:         playerName,
			Password:           password,
			ContentFingerprint: fingerprint,
		})
		if err != nil {
			lm.fail("Failed to create room: " + err.Error())
			return
		}
		lm.wsReadLoop(conn)
	})
	return nil
}

func (lm *LobbyManager) JoinRoom(roomID, playerName, password string) error {
	lm.Reset()
	lm.prepareEngineNetConnection()

	if strings.TrimSpace(playerName) == "" {
		playerName = sys.cfg.Netplay.PlayerName
	}
	fingerprint := sys.currentContentFingerprint()

	lm.mu.Lock()
	lm.state = "connecting"
	lm.isHost = false
	lm.roomID = strings.ToUpper(strings.TrimSpace(roomID))
	lm.mu.Unlock()

	SafeGo(func() {
		conn, err := lm.dialLobbyWS()
		if err != nil {
			lm.fail("Could not connect to lobby server: " + err.Error())
			return
		}
		lm.mu.Lock()
		lm.wsConn = conn
		lm.mu.Unlock()

		err = lm.sendWS(lobbyWSMessage{
			Type:               "join_room",
			RoomID:             roomID,
			PlayerName:         playerName,
			Password:           password,
			ContentFingerprint: fingerprint,
		})
		if err != nil {
			lm.fail("Failed to join room: " + err.Error())
			return
		}
		lm.wsReadLoop(conn)
	})
	return nil
}

func (lm *LobbyManager) deliverSyncBytes(data []byte) {
	lm.syncMu.Lock()
	conn := lm.tcpBridgeConn
	if conn == nil {
		lm.syncPreBuf = append(lm.syncPreBuf, data)
		lm.syncMu.Unlock()
		return
	}
	lm.syncMu.Unlock()
	_, _ = conn.Write(data)
}

func (lm *LobbyManager) deliverGGPOBytes(data []byte) {
	lm.udpMu.RLock()
	uConn := lm.udpBridgeConn
	target := lm.ggpoTarget
	lm.udpMu.RUnlock()
	if uConn != nil && target != nil {
		_, _ = uConn.WriteToUDP(data, target)
	}
}

func (lm *LobbyManager) wsReadLoop(conn *websocket.Conn) {
	for {
		msgType, rawData, err := conn.ReadMessage()
		if err != nil {
			lm.mu.Lock()
			st := lm.state
			lm.mu.Unlock()
			if st != "connected" && st != "idle" && st != "error" {
				lm.fail("Disconnected from lobby server")
			}
			return
		}

		if msgType == websocket.BinaryMessage {
			if len(rawData) < 2 {
				continue
			}
			prefix := rawData[0]
			payload := append([]byte(nil), rawData[1:]...)
			if prefix == wsRelayChannelSync {
				lm.deliverSyncBytes(payload)
			} else if prefix == wsRelayChannelGGPO {
				lm.deliverGGPOBytes(payload)
			}
			continue
		}

		var msg lobbyWSMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "room_created":
			lm.mu.Lock()
			lm.roomID = msg.RoomID
			if msg.Room != nil {
				lm.roomName = msg.Room.Name
			}
			if len(msg.ICEServers) > 0 {
				lm.serverICEServers = msg.ICEServers
			}
			lm.state = "waiting"
			lm.mu.Unlock()
			lobbyLogf("[Lobby] Room created: ID=%s Name=%q", msg.RoomID, lm.roomName)

		case "peer_joined":
			lm.mu.Lock()
			lm.peerName = msg.PeerName
			if len(msg.ICEServers) > 0 {
				lm.serverICEServers = msg.ICEServers
			}
			lm.state = "handshaking"
			lm.mu.Unlock()
			lobbyLogf("[Lobby] Challenger %q joined room %s; starting WebRTC offer...", msg.PeerName, msg.RoomID)
			if err := lm.initWebRTC(true); err != nil {
				lobbyLogf("[Lobby] WebRTC init error (%v), falling back to WebSocket relay", err)
				lm.activateWSRelay(true)
			}

		case "room_joined":
			lm.mu.Lock()
			lm.roomID = msg.RoomID
			if msg.Room != nil {
				lm.roomName = msg.Room.Name
			}
			lm.peerName = msg.PeerName
			if len(msg.ICEServers) > 0 {
				lm.serverICEServers = msg.ICEServers
			}
			lm.state = "handshaking"
			lm.mu.Unlock()
			lobbyLogf("[Lobby] Joined room %s (Host=%q); waiting for WebRTC offer...", msg.RoomID, msg.PeerName)
			if err := lm.initWebRTC(false); err != nil {
				lobbyLogf("[Lobby] WebRTC init error (%v), falling back to WebSocket relay", err)
				lm.activateWSRelay(true)
			}

		case "signal":
			var sig lobbySignalPayload
			if err := json.Unmarshal(msg.Payload, &sig); err == nil {
				lm.handleSignal(sig)
			}

		case "relay_fallback":
			lm.activateWSRelay(false)

		case "peer_left":
			lm.mu.Lock()
			st := lm.state
			isHost := lm.isHost
			lm.mu.Unlock()
			if st != "connected" {
				if isHost {
					lm.mu.Lock()
					lm.handshakeID++
					if lm.pc != nil {
						_ = lm.pc.Close()
						lm.pc = nil
					}
					lm.syncDC = nil
					lm.ggpoDC = nil
					lm.syncDCOpen = false
					lm.ggpoDCOpen = false
					lm.relayMode = false
					lm.bridgeStarted = false
					lm.remoteDescSet = false
					lm.pendingCandidates = nil
					lm.peerName = ""
					lm.state = "waiting"
					lm.mu.Unlock()
				} else {
					lm.fail(msg.Error)
				}
			}

		case "error":
			lm.fail(msg.Error)
		}
	}
}

func (lm *LobbyManager) buildICEConfig() webrtc.Configuration {
	stunURLs := make([]string, 0, len(sys.cfg.Netplay.STUNServers))
	for _, s := range sys.cfg.Netplay.STUNServers {
		s = strings.TrimSpace(s)
		if s != "" {
			stunURLs = append(stunURLs, s)
		}
	}
	if len(stunURLs) == 0 {
		stunURLs = []string{
			"stun:stun.cloudflare.com:3478",
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
		}
	}
	servers := []webrtc.ICEServer{
		{URLs: stunURLs},
	}

	lm.mu.Lock()
	srvICE := append([]LobbyICEServer(nil), lm.serverICEServers...)
	lm.mu.Unlock()
	for _, ice := range srvICE {
		if len(ice.URLs) == 0 {
			continue
		}
		entry := webrtc.ICEServer{URLs: ice.URLs}
		if ice.Username != "" || ice.Credential != "" {
			entry.Username = ice.Username
			entry.Credential = ice.Credential
			entry.CredentialType = webrtc.ICECredentialTypePassword
		}
		servers = append(servers, entry)
	}

	if turnURL := strings.TrimSpace(sys.cfg.Netplay.TURNServer); turnURL != "" {
		servers = append(servers, webrtc.ICEServer{
			URLs:           strings.Split(turnURL, ","),
			Username:       sys.cfg.Netplay.TURNUser,
			Credential:     sys.cfg.Netplay.TURNPassword,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return webrtc.Configuration{
		ICEServers: servers,
	}
}

func (lm *LobbyManager) sendSignal(sig lobbySignalPayload) {
	raw, err := json.Marshal(sig)
	if err != nil {
		return
	}
	_ = lm.sendWS(lobbyWSMessage{
		Type:    "signal",
		Payload: raw,
	})
}

func (lm *LobbyManager) activateWSRelay(notifyPeer bool) {
	lm.mu.Lock()
	if (lm.state != "handshaking" && lm.state != "connecting") || lm.bridgeStarted {
		lm.mu.Unlock()
		return
	}
	lm.handshakeID++
	lm.relayMode = true
	lm.bridgeStarted = true
	pc := lm.pc
	lm.pc = nil
	lm.mu.Unlock()

	if pc != nil {
		_ = pc.Close()
	}
	if notifyPeer {
		_ = lm.sendWS(lobbyWSMessage{Type: "relay_fallback"})
	}
	lobbyLogf("[Lobby/Relay] Strict NAT fallback activated: tunneling sync & GGPO over Lobby WebSocket")
	SafeGo(lm.startLoopbackBridges)
}

func (lm *LobbyManager) initWebRTC(isHost bool) error {
	pc, err := webrtc.NewPeerConnection(lm.buildICEConfig())
	if err != nil {
		return err
	}

	lm.mu.Lock()
	if lm.pc != nil {
		_ = lm.pc.Close()
	}
	lm.handshakeID++
	currentHandshakeID := lm.handshakeID
	lm.pc = pc
	lm.syncDCOpen = false
	lm.ggpoDCOpen = false
	lm.relayMode = false
	lm.bridgeStarted = false
	lm.remoteDescSet = false
	lm.pendingCandidates = nil
	lm.mu.Unlock()

	// Automatic timeout fallback: if strict CGNAT/LTE blocks UDP hole-punching for >6s, fall back to WebSocket Relay
	SafeGo(func() {
		time.Sleep(6 * time.Second)
		lm.mu.Lock()
		stillWaiting := lm.handshakeID == currentHandshakeID && lm.state == "handshaking" && !lm.bridgeStarted
		lm.mu.Unlock()
		if stillWaiting {
			lobbyLogf("[Lobby/WebRTC] UDP hole-punch timed out after 6s; engaging WebSocket Relay fallback")
			lm.activateWSRelay(true)
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand := c.ToJSON()
		lm.sendSignal(lobbySignalPayload{Candidate: &cand})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		lobbyLogf("[Lobby/WebRTC] PeerConnection state: %s", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			lobbyLogf("[Lobby/WebRTC] Direct P2P failed (strict NAT); engaging WebSocket Relay fallback")
			lm.activateWSRelay(true)
		}
	})

	if isHost {
		syncDC, err := pc.CreateDataChannel("ikemen-sync", nil)
		if err != nil {
			return err
		}
		ordered := false
		maxRetransmits := uint16(0)
		ggpoDC, err := pc.CreateDataChannel("ikemen-ggpo", &webrtc.DataChannelInit{
			Ordered:        &ordered,
			MaxRetransmits: &maxRetransmits,
		})
		if err != nil {
			return err
		}
		lm.bindSyncChannel(syncDC)
		lm.bindGGPOChannel(ggpoDC)

		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return err
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			return err
		}
		lm.sendSignal(lobbySignalPayload{SDP: &offer})
	} else {
		pc.OnDataChannel(func(d *webrtc.DataChannel) {
			switch d.Label() {
			case "ikemen-sync":
				lm.bindSyncChannel(d)
			case "ikemen-ggpo":
				lm.bindGGPOChannel(d)
			}
		})
	}
	return nil
}

func (lm *LobbyManager) handleSignal(sig lobbySignalPayload) {
	lm.mu.Lock()
	pc := lm.pc
	lm.mu.Unlock()
	if pc == nil {
		return
	}

	if sig.SDP != nil {
		if err := pc.SetRemoteDescription(*sig.SDP); err != nil {
			lobbyLogf("[Lobby/WebRTC] SetRemoteDescription error: %v", err)
			lm.activateWSRelay(true)
			return
		}
		lm.mu.Lock()
		lm.remoteDescSet = true
		pending := lm.pendingCandidates
		lm.pendingCandidates = nil
		isHost := lm.isHost
		lm.mu.Unlock()

		for _, c := range pending {
			_ = pc.AddICECandidate(c)
		}

		if !isHost && sig.SDP.Type == webrtc.SDPTypeOffer {
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				lm.activateWSRelay(true)
				return
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				lm.activateWSRelay(true)
				return
			}
			lm.sendSignal(lobbySignalPayload{SDP: &answer})
		}
	}

	if sig.Candidate != nil {
		lm.mu.Lock()
		ready := lm.remoteDescSet
		if !ready {
			lm.pendingCandidates = append(lm.pendingCandidates, *sig.Candidate)
		}
		lm.mu.Unlock()
		if ready {
			_ = pc.AddICECandidate(*sig.Candidate)
		}
	}
}

func (lm *LobbyManager) bindSyncChannel(dc *webrtc.DataChannel) {
	lm.mu.Lock()
	lm.syncDC = dc
	lm.mu.Unlock()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := append([]byte(nil), msg.Data...)
		lm.deliverSyncBytes(data)
	})

	dc.OnOpen(func() {
		lobbyLogf("[Lobby/WebRTC] DataChannel '%s' opened", dc.Label())
		lm.mu.Lock()
		lm.syncDCOpen = true
		ready := lm.syncDCOpen && lm.ggpoDCOpen && !lm.bridgeStarted
		if ready {
			lm.bridgeStarted = true
		}
		lm.mu.Unlock()
		if ready {
			SafeGo(lm.startLoopbackBridges)
		}
	})
}

func (lm *LobbyManager) bindGGPOChannel(dc *webrtc.DataChannel) {
	lm.mu.Lock()
	lm.ggpoDC = dc
	lm.mu.Unlock()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		lm.deliverGGPOBytes(msg.Data)
	})

	dc.OnOpen(func() {
		lobbyLogf("[Lobby/WebRTC] DataChannel '%s' opened", dc.Label())
		lm.mu.Lock()
		lm.ggpoDCOpen = true
		ready := lm.syncDCOpen && lm.ggpoDCOpen && !lm.bridgeStarted
		if ready {
			lm.bridgeStarted = true
		}
		lm.mu.Unlock()
		if ready {
			SafeGo(lm.startLoopbackBridges)
		}
	})
}

func (lm *LobbyManager) sendSyncOutbound(payload []byte) error {
	lm.mu.Lock()
	relay := lm.relayMode
	dc := lm.syncDC
	lm.mu.Unlock()
	if relay || dc == nil {
		return lm.sendWSBinary(wsRelayChannelSync, payload)
	}
	return dc.Send(payload)
}

func (lm *LobbyManager) sendGGPOOutbound(payload []byte) error {
	lm.mu.Lock()
	relay := lm.relayMode
	dc := lm.ggpoDC
	lm.mu.Unlock()
	if relay || dc == nil {
		return lm.sendWSBinary(wsRelayChannelGGPO, payload)
	}
	return dc.Send(payload)
}

func pickFreeLocalUDPPort(preferred int, isHost bool) int {
	if !isHost && preferred > 0 && preferred < 65535 {
		preferred++
	}
	if preferred > 0 && preferred <= 65535 {
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: preferred}); err == nil {
			_ = c.Close()
			return preferred
		}
	}
	for p := 7501; p <= 7650; p++ {
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: p}); err == nil {
			_ = c.Close()
			return p
		}
	}
	if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}); err == nil {
		port := c.LocalAddr().(*net.UDPAddr).Port
		_ = c.Close()
		return port
	}
	return preferred
}

func (lm *LobbyManager) startLoopbackBridges() {
	lm.mu.Lock()
	isHost := lm.isHost
	relay := lm.relayMode
	syncDC := lm.syncDC
	ggpoDC := lm.ggpoDC
	lm.mu.Unlock()

	if !relay && (syncDC == nil || ggpoDC == nil) {
		lm.fail("WebRTC data channels closed unexpectedly")
		return
	}

	// 1. Setup local UDP loopback proxy for GGPO rollback
	localGGPOPort := pickFreeLocalUDPPort(sys.cfg.Netplay.Rollback.Port, isHost)
	udpProxy, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		lm.fail("Failed to bind local UDP rollback bridge: " + err.Error())
		return
	}
	udpProxyPort := udpProxy.LocalAddr().(*net.UDPAddr).Port
	if udpProxyPort == localGGPOPort {
		localGGPOPort = pickFreeLocalUDPPort(localGGPOPort+1, isHost)
	}

	lm.udpMu.Lock()
	lm.udpBridgeConn = udpProxy
	lm.localGGPOPort = localGGPOPort
	lm.udpProxyPort = udpProxyPort
	lm.ggpoTarget = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localGGPOPort}
	lm.udpMu.Unlock()

	// Pump local GGPO UDP -> Outbound (WebRTC DataChannel or WebSocket Binary Relay)
	SafeGo(func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := udpProxy.ReadFromUDP(buf)
			if err != nil {
				return
			}
			lm.udpMu.Lock()
			lm.ggpoTarget = from
			lm.udpMu.Unlock()
			payload := append([]byte(nil), buf[:n]...)
			_ = lm.sendGGPOOutbound(payload)
		}
	})

	// 2. Setup local TCP loopback pair for NetConnection ('ikemen-sync')
	tcpLn, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		lm.fail("Failed to bind local TCP sync bridge: " + err.Error())
		return
	}
	tcpAddr := tcpLn.Addr().String()

	var bridgeConn *net.TCPConn
	var acceptErr error
	acceptDone := make(chan struct{})
	SafeGo(func() {
		defer close(acceptDone)
		defer tcpLn.Close()
		bridgeConn, acceptErr = tcpLn.AcceptTCP()
	})

	rawClient, err := net.DialTimeout("tcp4", tcpAddr, 3*time.Second)
	if err != nil {
		lm.fail("Failed to dial local TCP sync bridge: " + err.Error())
		return
	}
	engineTCPConn := rawClient.(*net.TCPConn)
	<-acceptDone
	if acceptErr != nil || bridgeConn == nil {
		_ = engineTCPConn.Close()
		lm.fail("Failed to accept local TCP sync bridge")
		return
	}

	// Flush any pre-buffered sync bytes and wire bridgeConn
	lm.syncMu.Lock()
	for _, chunk := range lm.syncPreBuf {
		_, _ = bridgeConn.Write(chunk)
	}
	lm.syncPreBuf = nil
	lm.tcpBridgeConn = bridgeConn
	lm.syncMu.Unlock()

	// Pump bridgeConn -> Outbound (WebRTC DataChannel or WebSocket Binary Relay)
	SafeGo(func() {
		buf := make([]byte, 16384)
		for {
			n, err := bridgeConn.Read(buf)
			if n > 0 {
				payload := append([]byte(nil), buf[:n]...)
				if sErr := lm.sendSyncOutbound(payload); sErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = bridgeConn.Close()
	})

	// 3. Complete end-to-end "IKEMENGO" magic verification over the tunnel
	if err := sys.netConnection.AttachWebRTC(engineTCPConn, isHost, localGGPOPort, udpProxyPort); err != nil {
		_ = engineTCPConn.Close()
		_ = bridgeConn.Close()
		lm.fail("P2P handshake verification failed: " + err.Error())
		return
	}

	lm.mu.Lock()
	lm.state = "connected"
	lm.mu.Unlock()

	if isHost {
		_ = lm.sendWS(lobbyWSMessage{Type: "match_started"})
	}
	lobbyLogf("[Lobby] Tunnel active! mode=%s isHost=%v localGGPOPort=%d udpProxyPort=%d",
		map[bool]string{false: "WebRTC-P2P", true: "WS-Relay"}[relay], isHost, localGGPOPort, udpProxyPort)
}

func (nc *NetConnection) AttachWebRTC(tcpConn *net.TCPConn, isHost bool, localGGPOPort, udpProxyPort int) error {
	if nc == nil || nc.isClosing() {
		return fmt.Errorf("netConnection is closing")
	}
	nc.host = isHost
	nc.conn = nil
	if isHost {
		nc.locIn, nc.remIn = nc.GetHostGuestRemap()
	} else {
		nc.remIn, nc.locIn = nc.GetHostGuestRemap()
	}

	_ = tcpConn.SetDeadline(time.Now().Add(10 * time.Second))
	if isHost {
		if _, err := tcpConn.Write([]byte("IKEMENGO")); err != nil {
			return err
		}
		ack := make([]byte, 8)
		if _, err := io.ReadFull(tcpConn, ack); err != nil || string(ack) != "IKEMENGO" {
			return fmt.Errorf("invalid guest ack: %v", err)
		}
	} else {
		buf := make([]byte, 8)
		if _, err := io.ReadFull(tcpConn, buf); err != nil || string(buf) != "IKEMENGO" {
			return fmt.Errorf("invalid host handshake: %v", err)
		}
		if _, err := tcpConn.Write([]byte("IKEMENGO")); err != nil {
			return err
		}
	}
	_ = tcpConn.SetDeadline(time.Time{})

	nc.webrtcMode = true
	nc.webrtcLocalGGPOPort = localGGPOPort
	nc.webrtcUDPProxyPort = udpProxyPort
	if sys.cfg.Netplay.RollbackNetcode && sys.rollback.session != nil {
		sys.rollback.session.remoteIp = "127.0.0.1"
		if isHost {
			sys.rollback.session.host = ""
		} else {
			sys.rollback.session.host = "127.0.0.1"
		}
	}
	nc.conn = tcpConn
	return nil
}

// ============================================================================
// Embedded In-Process Lobby & Signaling Server (Auto-started for Local/LAN)
// ============================================================================

type embeddedClient struct {
	conn   *websocket.Conn
	sendMu sync.Mutex
	roomID string
	isHost bool
	name   string
}

func (c *embeddedClient) sendJSON(msg lobbyWSMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

func (c *embeddedClient) sendBinary(data []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

type embeddedRoom struct {
	info     LobbyRoomInfo
	password string
	host     *embeddedClient
	guest    *embeddedClient
}

type embeddedLobbyHub struct {
	mu    sync.RWMutex
	rooms map[string]*embeddedRoom
}

func generateEmbeddedRoomID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func (hub *embeddedLobbyHub) getPeer(c *embeddedClient) *embeddedClient {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	if c.roomID == "" {
		return nil
	}
	r, ok := hub.rooms[c.roomID]
	if !ok {
		return nil
	}
	if c.isHost {
		return r.guest
	}
	return r.host
}

func (hub *embeddedLobbyHub) removeClient(c *embeddedClient) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if c.roomID == "" {
		return
	}
	r, ok := hub.rooms[c.roomID]
	c.roomID = ""
	if !ok {
		return
	}
	if r.host == c {
		guest := r.guest
		delete(hub.rooms, r.info.ID)
		if guest != nil {
			guest.roomID = ""
			_ = guest.sendJSON(lobbyWSMessage{
				Type:  "peer_left",
				Error: "Host closed the lobby",
			})
		}
	} else if r.guest == c {
		r.guest = nil
		r.info.GuestName = ""
		r.info.PlayerCount = 1
		r.info.Status = "waiting"
		if r.host != nil {
			_ = r.host.sendJSON(lobbyWSMessage{
				Type:   "peer_left",
				RoomID: r.info.ID,
				Room:   &r.info,
				Error:  "Challenger left the lobby",
			})
		}
	}
}

func startEmbeddedLobbyServer(listenAddr string) {
	embeddedLobbyMu.Lock()
	if embeddedLobbyRunning {
		embeddedLobbyMu.Unlock()
		return
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		embeddedLobbyMu.Unlock()
		return
	}
	embeddedLobbyRunning = true
	embeddedLobbyMu.Unlock()

	hub := &embeddedLobbyHub{
		rooms: make(map[string]*embeddedRoom),
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return true },
		ReadBufferSize:  32768,
		WriteBufferSize: 32768,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","mode":"embedded"}`))
	})
	mux.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hub.mu.RLock()
		list := make([]LobbyRoomInfo, 0, len(hub.rooms))
		for _, rm := range hub.rooms {
			list = append(list, rm.info)
		}
		hub.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"rooms": list})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &embeddedClient{conn: conn}
		defer func() {
			hub.removeClient(client)
			_ = conn.Close()
		}()

		for {
			msgType, rawData, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage {
				if peer := hub.getPeer(client); peer != nil {
					_ = peer.sendBinary(rawData)
				}
				continue
			}
			var msg lobbyWSMessage
			if err := json.Unmarshal(rawData, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "create_room":
				hub.removeClient(client)
				hub.mu.Lock()
				id := generateEmbeddedRoomID()
				for hub.rooms[id] != nil {
					id = generateEmbeddedRoomID()
				}
				rName := strings.TrimSpace(msg.RoomName)
				if rName == "" {
					rName = "Ikemen Arena #" + id
				}
				pName := strings.TrimSpace(msg.PlayerName)
				if pName == "" {
					pName = "Player 1"
				}
				client.name = pName
				client.roomID = id
				client.isHost = true
				info := LobbyRoomInfo{
					ID:                 id,
					Name:               rName,
					HostName:           pName,
					HasPassword:        msg.Password != "",
					Status:             "waiting",
					ContentFingerprint: msg.ContentFingerprint,
					PlayerCount:        1,
					MaxPlayers:         2,
					CreatedAt:          time.Now().UTC(),
				}
				hub.rooms[id] = &embeddedRoom{
					info:     info,
					password: msg.Password,
					host:     client,
				}
				hub.mu.Unlock()
				_ = client.sendJSON(lobbyWSMessage{
					Type:   "room_created",
					RoomID: id,
					Room:   &info,
				})

			case "join_room":
				hub.removeClient(client)
				hub.mu.Lock()
				rID := strings.ToUpper(strings.TrimSpace(msg.RoomID))
				rm, ok := hub.rooms[rID]
				if !ok {
					hub.mu.Unlock()
					_ = client.sendJSON(lobbyWSMessage{Type: "error", Error: "Lobby room not found: " + rID})
					continue
				}
				if rm.guest != nil || rm.info.Status != "waiting" {
					hub.mu.Unlock()
					_ = client.sendJSON(lobbyWSMessage{Type: "error", Error: "Lobby room is already full or in a match"})
					continue
				}
				if rm.password != "" && rm.password != msg.Password {
					hub.mu.Unlock()
					_ = client.sendJSON(lobbyWSMessage{Type: "error", Error: "Incorrect lobby password"})
					continue
				}
				if rm.info.ContentFingerprint != "" && msg.ContentFingerprint != "" && rm.info.ContentFingerprint != msg.ContentFingerprint {
					hub.mu.Unlock()
					_ = client.sendJSON(lobbyWSMessage{
						Type:  "error",
						Error: "Game content/roster mismatch with host (" + rm.info.ContentFingerprint[:8] + " vs " + msg.ContentFingerprint[:8] + ")",
					})
					continue
				}
				pName := strings.TrimSpace(msg.PlayerName)
				if pName == "" {
					pName = "Challenger"
				}
				client.name = pName
				client.roomID = rID
				client.isHost = false
				rm.guest = client
				rm.info.GuestName = pName
				rm.info.PlayerCount = 2
				rm.info.Status = "handshaking"
				infoCopy := rm.info
				hostClient := rm.host
				hub.mu.Unlock()

				_ = client.sendJSON(lobbyWSMessage{
					Type:     "room_joined",
					RoomID:   rID,
					Room:     &infoCopy,
					PeerName: infoCopy.HostName,
				})
				if hostClient != nil {
					_ = hostClient.sendJSON(lobbyWSMessage{
						Type:     "peer_joined",
						RoomID:   rID,
						Room:     &infoCopy,
						PeerName: pName,
					})
				}

			case "signal", "relay_fallback":
				if peer := hub.getPeer(client); peer != nil {
					_ = peer.sendJSON(lobbyWSMessage{
						Type:    msg.Type,
						RoomID:  client.roomID,
						Payload: msg.Payload,
					})
				}

			case "match_started":
				hub.mu.Lock()
				if rm, ok := hub.rooms[client.roomID]; ok && client.isHost {
					rm.info.Status = "in_game"
				}
				hub.mu.Unlock()
			}
		}
	})

	lobbyLogf("[Lobby] Auto-started embedded Lobby Server on http://%s", listenAddr)
	SafeGo(func() {
		_ = http.Serve(ln, mux)
	})
}
