# Wintun Peer-to-Peer UDP Hole Punching

A toy peer-to-peer VPN tunnel for Windows using UDP hole punching and the [Wintun](https://www.wintun.net/) TUN driver. 
Two peers behind NAT can establish a direct encrypted tunnel without any relay server after the initial STUN handshake.

---

## How It Works

1. A **STUN server** (`server.go`) running on a public IP helps both peers discover each other's public endpoints.
2. Each **peer** (`peer.go`) registers with the STUN server and receives the other peer's public IP/port.
3. UDP hole punching is used to establish a direct peer-to-peer connection.
4. A **Wintun TUN interface** is created on each Windows machine, allowing IP traffic to flow over the tunnel.

```
 [PC 1]  <──── UDP Hole Punch ────>  [PC 2]
    \                                   /
     \──── register / discover ────────/
                   |
            [STUN Server]
           (public IP required)
```

---

## Prerequisites

| Requirement | Details |
|---|---|
| **OS** | Windows (peer) / Linux or Windows (server) |
| **Go** | 1.18 or later |
| **wintun.dll** | Must be placed alongside the peer binary |
| **Admin privileges** | Required on Windows to create TUN interfaces |

> Download `wintun.dll` from [https://www.wintun.net](https://www.wintun.net) and place it in the same directory as the compiled `peer.exe`.

---

## Project Structure

```
.
├── server.go       # STUN server — run on a machine with a public IP
├── peer.go         # Peer node — run on both Windows PCs
└── wintun.dll      # Wintun driver DLL (must be present at runtime)
```

---

## Setup & Configuration

### 1. STUN Server (`server.go`)

Run this on any machine with a **public IP address** (a VPS works perfectly).

```bash
go run server.go
```

No configuration changes are required unless you want to change the listening port (default: `2020`).

---

### 2. Peer Configuration (`peer.go`)

Edit the constants at the top of `peer.go` before building. Configuration differs slightly between PC 1 and PC 2.

#### PC 1 Configuration

```go
STUN_SERVER = "192.168.30.6"  // Public IP of the machine running server.go
STUN_PORT   = 2020            // Must match the server's listening port
PUB_KEY     = 43              // Shared key — must be the same on both peers
TUN_NAME    = "melson"        // Name of the virtual TUN interface
TUN_LOCAL   = "100.64.1.1"   // Tunnel IP for THIS machine (PC 1)
TUN_REMOTE  = "100.64.1.2"   // Tunnel IP for the REMOTE machine (PC 2)
TUN_MTU     = 1400            // MTU for the tunnel interface
SRC_PORT    = 9999            // Local UDP source port
```

#### PC 2 Configuration

```go
STUN_SERVER = "192.168.30.6"  // Public IP of the machine running server.go
STUN_PORT   = 2020            // Must match the server's listening port
PUB_KEY     = 43              // Shared key — must be the same on both peers
TUN_NAME    = "melson"        // Name of the virtual TUN interface
TUN_LOCAL   = "100.64.1.2"   // Tunnel IP for THIS machine (PC 2)
TUN_REMOTE  = "100.64.1.1"   // Tunnel IP for the REMOTE machine (PC 1)
TUN_MTU     = 1400            // MTU for the tunnel interface
SRC_PORT    = 9999            // Local UDP source port
```

> **Note:** `TUN_LOCAL` and `TUN_REMOTE` are **swapped** between PC 1 and PC 2. `PUB_KEY` must be **identical** on both peers.

---

## Running

### Step 1 — Start the STUN server
```bash
# On your public server
go run server.go
```

### Step 2 — Build and run the peer on each Windows PC
```bash
# Run as Administrator on both PCs
go run peer.go
```

Or build first:
```bash
go build -o peer.exe peer.go
./peer.exe   # Must run as Administrator
```

### Step 3 — Verify the tunnel

Once both peers are connected, test the tunnel with a ping and can access device using ssh or access TCP services:

```bash
# From PC 1
ping 100.64.1.2

# From PC 2
ping 100.64.1.1
```

---

## Configuration Reference

| Parameter | Description | Example |
|---|---|---|
| `STUN_SERVER` | Public IP of the STUN server | `"203.0.113.10"` |
| `STUN_PORT` | STUN server UDP port | `2020` |
| `PUB_KEY` | Shared identifier key (same on both peers) | `43` |
| `TUN_NAME` | Name of the Wintun TUN adapter | `"melson"` |
| `TUN_LOCAL` | This peer's virtual tunnel IP | `"100.64.1.1"` |
| `TUN_REMOTE` | Remote peer's virtual tunnel IP | `"100.64.1.2"` |
| `TUN_MTU` | MTU of the tunnel interface | `1400` |
| `SRC_PORT` | Local UDP source port | `9999` |

---

## Troubleshooting

- **`wintun.dll` not found** — Make sure `wintun.dll` is in the same directory as `peer.exe`.
- **Interface creation fails** — Ensure you are running the peer binary as **Administrator**.
- **Peers can't find each other** — Confirm the STUN server is reachable from both peers and the firewall allows UDP on `STUN_PORT` and `SRC_PORT`.
- **Ping fails after connection** — Check that `TUN_LOCAL` / `TUN_REMOTE` are correctly swapped between the two peers.

---

## References

- [Wintun Go API Docs](https://pkg.go.dev/golang.zx2c4.com/wintun#section-readme)
- [Wintun Go Source](https://git.zx2c4.com/wintun-go/tree/?id=0fa3db229ce2)
- [Blog Post — Part 1: Peer-to-Peer on Windows](https://www.0xmm.in/posts/peer-to-peer-windows-part1/)
- [Blog Post — Part 2: Peer-to-Peer on Windows](https://www.0xmm.in/posts/peer-to-peer-windows-part2/)

---


