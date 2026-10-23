<p align="center">
  <a href="https://ikemen-engine.github.io/">
    <img src="https://github.com/user-attachments/assets/0dcd7ae1-5c9d-44e1-aa32-9ec4b9ed3952" style="width: 699px;" alt="IKEMEN GO Logo"/>
  </a>
</p>

# Ikemen GO — Online Lobby Edition (`v1.0.0-online`)

**Ikemen GO (Online Lobby Edition)** is an open-source fighting game engine compatible with [M.U.G.E.N](https://en.wikipedia.org/wiki/Mugen_(game_engine)) resources, built on top of [Ikemen GO](https://github.com/ikemen-engine/Ikemen-GO) and enhanced with **native, zero-port-forwarding Online Lobbies** out of the box.

---

## ✨ What's New in This Build

In addition to all standard **Ikemen GO v1.0.0** features (rollback netcode, ZSS scripting, Lua modules, 3D stage models, Vulkan/OpenGL 3.3 renderers, and M.U.G.E.N 1.1 compatibility), this build includes a complete built-in matchmaking and room system:

- **Create Public or Code-Based Lobbies (`CREATE LOBBY`)**: Host a room with a custom name and automatically receive a short shareable room code.
- **Live Lobby Browser (`BROWSE LOBBIES`)**: Browse open rooms hosted by other players and join with one press.
- **Join by Room Code (`JOIN BY ROOM CODE`)**: Enter a friend's room code directly for instant private matches.
- **3-Tier NAT Traversal (No Port Forwarding Required)**:
  1. **WebRTC P2P (STUN)**: Direct peer-to-peer UDP hole-punching via Google & Cloudflare STUN servers.
  2. **Dynamic TURN Relay**: Automatic TURN fallback when configured on the signaling server.
  3. **Strict-NAT WebSocket Relay (`ws_relay`)**: Seamless binary packet relay fallback over WebSockets so players behind symmetric/strict NATs or carrier-grade NAT (CGNAT) can still connect and play rollback netplay without forwarding ports.
- **Classic Direct IP (`DIRECT IP HOST` / `DIRECT IP JOIN`)**: Retains standard Ikemen GO direct IP hosting and joining over TCP/UDP port `7500`.
- **Bundled `lobby-server.exe`**: Includes a standalone, lightweight signaling & relay server binary for LAN events, local testing, or self-hosting.

---

## 🚀 Quick Start Guide (For Players)

### 1. Launching the Game
1. Download and extract `Ikemen_GO_Online-v1.0.0-windows.zip` to any folder on your PC.
2. Double-click **`Ikemen_GO.exe`** to launch the engine.
   *(Note: Keep the `lib/`, `data/`, `external/`, `chars/`, `stages/`, and `font/` folders alongside `Ikemen_GO.exe`.)*

### 2. Setting Up Controls & Display
- From the main menu, go to **OPTIONS**:
  - **Input Settings -> Key Config / Joystick Config**: Map your keyboard keys or controller buttons for Player 1 and Player 2.
  - **Video Settings**: Change resolution, toggle Fullscreen (`Alt+Enter`), VSync, or switch between OpenGL 3.3 and Vulkan 1.3.
- Select **Save and Return** when finished.

### 3. Playing Online
From the title screen, select **ONLINE**:
- **CREATE LOBBY**: Type a room name (or press `Enter` for the default) to open a lobby. Your **Room Code** will be displayed on screen while waiting for a challenger.
- **BROWSE LOBBIES**: View a list of currently open lobbies. Select any room to join, or select **REFRESH LOBBY LIST** to update.
- **JOIN BY ROOM CODE**: Type the short code shared by the host and press `Enter` to connect directly.
- **DIRECT IP HOST / DIRECT IP JOIN**: Connect directly via IPv4 address (requires the host to forward TCP & UDP port `7500` or use a virtual LAN tool such as Radmin VPN / ZeroTier).

> **Tip — Setting Your Online Nickname:**
> Open `save/config.ini` (generated after first launch) and under `[Netplay]` change:
> ```ini
> PlayerName = YourNickname
> ```

---

## 🛠️ Guide for Game Creators & M.U.G.E.N Porters

### Adding Characters & Stages
1. **Characters**: Place character folders inside `chars/` (e.g., `chars/mychar/mychar.def`), then register them under `[Characters]` in `data/select.def`.
2. **Stages**: Place stage `.def` and `.sff` files inside `stages/`, then list them under `[ExtraStages]` in `data/select.def`.

### Adding Online Lobby Menus to a Custom Screenpack (`system.def`)
The bundled default motif (`data/ikemen1/system.def`) already includes the online lobby menu items. If you are porting a custom M.U.G.E.N 1.0/1.1 or Ikemen screenpack, add the following lines under `[Title Info]` in your `system.def`:

```ini
; Submenu: Online Lobbies & Netplay
menu.itemname.menunetwork = "ONLINE"
menu.itemname.menunetwork.lobbycreate = "CREATE LOBBY"
menu.itemname.menunetwork.lobbybrowse = "BROWSE LOBBIES"
menu.itemname.menunetwork.lobbyjoinid = "JOIN BY ROOM CODE"
menu.itemname.menunetwork.serverhost = "DIRECT IP HOST"
menu.itemname.menunetwork.serverjoin = "DIRECT IP JOIN"
menu.itemname.menunetwork.serverjoin.joinadd = "NEW ADDRESS"
menu.itemname.menunetwork.serverjoin.back = "BACK"
menu.itemname.menunetwork.back = "BACK"
```

### Configuring or Self-Hosting the Lobby Server
By default, the engine connects to the preconfigured cloud signaling server in `save/config.ini`:
```ini
[Netplay]
LobbyServer = wss://ikemen-go-lobby.onrender.com/ws
PlayerName  = Player
```
To host your own lobby signaling & relay server:
1. Run `lobby-server.exe` on your server or local machine (defaults to port `8080`, or set the `PORT` environment variable).
2. Point `LobbyServer` in `save/config.ini` to `ws://<your-server-ip>:8080/ws` (or `wss://...` behind TLS).
3. A ready-to-deploy `Dockerfile.lobby` and `render.yaml` are included in the source repository.

---

## 💻 System Requirements

| Platform | Minimum Requirements |
|---|---|
| **OS** | Windows 10 / 11 (64-bit) |
| **GPU** | Graphics card supporting OpenGL 3.3 or Vulkan 1.3 |
| **Network** | Broadband internet connection for rollback netplay |

---

## 🏆 Credits & Acknowledgements

This release stands on the shoulders of decades of community work across the M.U.G.E.N and IKEMEN ecosystems:

### M.U.G.E.N
- **[Elecbyte](https://en.wikipedia.org/wiki/Mugen_(game_engine))** — Original creators of the **M.U.G.E.N** fighting game engine, the *Kung Fu Man* (`kfm`) character, sample stages (`stage0`, `kfm`), and legacy M.U.G.E.N fonts.

### IKEMEN & Ikemen GO Engine
- **Suehiro (`supersuehiro`)** — Original creator of the **IKEMEN** and **[Ikemen GO](https://osdn.net/users/supersuehiro/pf/ikemen_go/)** engine.
- **[K4thos](https://github.com/K4thos) & the [Ikemen GO Development Team & Contributors](https://github.com/ikemen-engine/Ikemen-GO)** — Lead architecture, M.U.G.E.N compatibility, ZSS state scripting, rollback netcode integration, Vulkan/OpenGL renderers, and ongoing engine development:
  - *Suehiro, K4thos, NeatUnsou, ShinLucho, Daniel Porto, Gacel, Nikolay Griva, Wintermourn, Amavect, BwdYeti, Eiton, Super, two4teezee, fantasma, GaryCXJk, PotS, Sam Hocevar, Wreq!, Foobs, RollinJay, Wftk, wily-coyote, Jesuszilla, Kasasagi, Orange-Dolphin, Rakíel, Alceris, Kase, Leon Kasovan, Kokorz, and all Ikemen GO contributors.*

### Community Tools, Tweaks & Ecosystem
- **[CableDorado2 (CD2)](https://github.com/CableDorado2)** — Special credit and gratitude for his extensive contributions to the IKEMEN community, including *Ikemen Plus Ultra*, *Ikemen GO GameModes & Tweaks*, Lua modules, tutorials, and resources that have helped countless developers build and customize IKEMEN games.

### Default Screenpack (`IKEMEN1`) & Bundled Assets
- **[Ikemen-GO-Screenpack](https://github.com/ikemen-engine/Ikemen-GO-Screenpack)** contributors:
  - **Ohmga Shironeko** (Screenpack motif, lifebar assets & logo)
  - **SuperFromND** (Screenpack motif sounds)
  - **President Devon** (Screenpack logo, lifebar messages, rank backgrounds & action icons)
  - **Rurouni** (Lifebar messages, command list glyphs, order select icons & title logos)
  - **Shiyo Kakuge** (Dizzy, guard break & tag switch effects)
  - **Cylia Margatroid** (Engine icon/logo & title screen motif logos)
  - **Gacel** (`default-3x5` system font)
  - **Miguel Young** (Lifebar voice lines)

### Online Lobby Fork (`Ikemen-GO-Online`)
- **MUZI-THE-BUSHI ([TheWiredWarrior051](https://github.com/TheWiredWarrior051))** — Native WebRTC P2P online lobby system, strict-NAT WebSocket relay fallback, room browser/code matchmaking, and standalone lobby signaling server.

---

## 📄 License

- The **Ikemen GO** engine and Lua scripts are licensed under the **MIT License**.
- Bundled **Ikemen-GO-Screenpack** assets are licensed under **Creative Commons Attribution 3.0 (CC BY 3.0)**.
- **Elecbyte's** sample M.U.G.E.N content (`kfm`, `stage0`, M.U.G.E.N fonts) is subject to Elecbyte's non-commercial license terms.
- This program dynamically links **FFmpeg** under the **LGPL v2.1** license.
- See [`LICENSES.txt`](./LICENSES.txt) for full license texts and per-component notices.
