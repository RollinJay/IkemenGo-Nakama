# GGPO transport extension required by IKEMEN GO Nakama integration

The bundled `third_party/ggpo` source includes the exact transport change needed by
IKEMEN GO's Nakama P2P integration.

## Added API

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

The constructor reuses the normal GGPO UDP transport initialization and takes
ownership of the supplied `net.PacketConn`. It never binds a second socket.

The existing constructor remains:

```go
func NewUdp(messageHandler MessageHandler, localPort int) (Udp, error)
```

and delegates to the same internal initialization path after binding its own
socket.

## Required semantics

- The supplied listener remains the transport's actual `net.PacketConn`.
- No call to `net.ListenPacket`/`net.ListenUDP` occurs for `NewUdpFromPacketConn`.
- The normal GGPO sender goroutine is started once.
- `Udp.Close()` closes the transferred listener through the existing shared
  close-once lifecycle.
- The supplied local port is not rebound or changed.
- The transport continues to use the existing `net.PacketConn` I/O path.

## IKEMEN integration sequence

```text
Nakama match join
    -> STUN candidate gathering
    -> UDP hole punch
    -> successful hello/ack
    -> NakamaP2P retains socket + observed remote endpoint
    -> RollbackSystem pre-match setup
    -> TakeP2PTransport
    -> NewUdpFromPacketConn
    -> GGPO InitializeConnection
```

The GGPO remote player uses the endpoint observed by the successful P2P
handshake, not the peer's TCP/bootstrap rollback port.

## Verification performed

The bundled GGPO transport package passes its normal lifecycle tests plus the
new `NewUdpFromPacketConn` test covering:

1. use of the original bound socket;
2. unchanged local port;
3. packet delivery through the supplied socket;
4. close/unblock behavior;
5. successful rebind after GGPO closes the transferred socket.

The core GGPO packages also compile in an isolated Go 1.23 validation copy after
substituting only the unavailable `x/exp/constraints` package. Full engine
build validation still requires an actual Go 1.27.x toolchain and the project's
other native/build dependencies.
