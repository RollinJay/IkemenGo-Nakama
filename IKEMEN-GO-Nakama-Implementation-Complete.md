# IKEMEN GO Nakama — Complete Implementation Record

This document combines the implementation records from Pass 5 (recovered) through Pass 12 into one chronological reference. The pass documents are preserved in order; this is a consolidation, not a rewrite of their contents.



---

# Pass 5

> Source record: `/mnt/data/IKEMEN-GO-Nakama-Implementation-pass5-recovered.md`

# IKEMEN GO + Nakama Matchmaking Integration

## 1. Overview

This project adds a Nakama-backed online service layer to IKEMEN GO while retaining IKEMEN GO's existing deterministic simulation, GGPO rollback system, UDP gameplay transport, replay system, Lua scripting model, and screenpack/motif system as the primary game-side machinery.

The intended architecture is:

```text
                         NAKAMA
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   Authentication      Matchmaking         Lobbies
        │                  │                  │
        │             Ranked/Unranked     Tournament
        │                  │              Scheduling
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │
                     Match/session
                           │
                 P2P signaling / setup
                           │
              ┌────────────┴────────────┐
              │                         │
           Player A                  Player B
              │                         │
              └──── existing GGPO ─────┘
                           │
                   deterministic replay
                           │
                     600-frame delay
                           │
                           ▼
                        Nakama
                           │
                 delayed input stream
                           │
                   ┌───────┼───────┐
                   ▼       ▼       ▼
               spectator spectator spectator
                   │       │       │
                   └── IKEMEN deterministic playback ──┘
```

Nakama is intended to be the control plane and social/matchmaking service. It is not intended to become the live fighting-game simulation server.

## 2. Implemented Features

### 2.1 Nakama client integration

Added an engine-side Nakama client in `src/nakama_client.go`.

Implemented capabilities include:

- REST authentication using device/session credentials.
- Realtime WebSocket connection.
- WebSocket read/write loops.
- Event dispatch from Nakama into IKEMEN.
- Match join/leave.
- Match data send operations.
- Server RPC calls.
- Lobby listing through Nakama authoritative match listing.
- Chat channel join/send.
- P2P signaling messages.
- Replay header/chunk/end/reset messages.

The client is exposed to Lua through `src/nakama_script.go`.

### 2.2 Ranked matchmaking

The Nakama server module `nakama/modules/ikemen_matchmaker.lua` implements ranked and unranked matchmaking.

Ranked matchmaking:

- Groups tickets by game, build, mode, and region.
- Retrieves the player's rating from server-side storage.
- Compares rating distance against the configured allowed range.
- Chooses a compatible opponent.
- Creates a Nakama `ikemen_session` match for the two players.

The client can provide an Elo range as matchmaking metadata, but the authoritative rating is intended to come from server-side storage.

### 2.3 Unranked matchmaking

Unranked matchmaking uses the same matchmaker infrastructure while ignoring Elo distance when pairing compatible tickets.

The matchmaking target is two players.

### 2.4 Server-side Elo

Added `nakama/modules/ikemen_rating.lua`.

The module provides:

- Per-game rating storage.
- Configurable initial rating.
- Configurable K-factor.
- Configurable rating scale.
- Wins/losses/game counters.
- Reciprocal result submission.
- Match-specific settlement protection so the same match is not intended to change ratings twice.
- Optional replay hash agreement checking.

Default configuration currently is:

```lua
initial_rating = 1000
k_factor = 32
rating_scale = 400
```

Game-specific values can be placed in the `GAME_CONFIG` table.

This is not a cryptographic anti-cheat system. It verifies reciprocal result claims, but two cooperating clients could still submit a false matching result unless a separate authoritative replay verification system is added.

### 2.5 Authoritative 8-player lobbies

Added `nakama/modules/ikemen_lobby.lua`.

A lobby supports up to eight players and keeps authoritative state for:

- Player membership/order.
- Lobby name.
- Game/build identity.
- Lobby format.
- Rules metadata.
- Current pairing.
- Tournament state.
- Result progression.

Supported scheduling formats are:

```text
winner_stays_on
round_robin
bracket
```

The lobby coordinates pairings but does not simulate the fight.

### 2.6 Lobby browser

The Lua API exposes `nakama.listLobbies()` through Nakama's match listing API.

The lobby browser is intentionally not implemented as a separate hardcoded renderer. Screenpacks/game scripts can use the returned lobby data to construct their own interface using normal IKEMEN Lua/UI facilities.

### 2.7 Nakama chat

Chat access is provided through the Nakama client:

```lua
nakama.joinChat(target)
nakama.sendChat(channel_id, message)
```

Incoming chat events are exposed to Lua through the Nakama event callback system.

### 2.8 P2P / NAT traversal primitive

Added `src/nakama_p2p.go`.

The current implementation provides:

- Local IPv4 candidate discovery.
- STUN Binding Request support.
- Server-reflexive address discovery.
- UDP hole-punching.
- Shared handshake token validation.
- Persistent UDP socket retention after the P2P connection succeeds.

The P2P object exposes the established UDP connection and remote endpoint so it can eventually be attached to the GGPO transport without closing/rebinding the NAT mapping.

Lua access is currently exposed as:

```lua
nakama.startP2P(port, stun_servers)
nakama.stopP2P()
```

The player is not expected to manually configure the connection method in the intended final flow.

## 3. Existing IKEMEN/GGPO Systems Retained

The implementation deliberately leaves the following existing mechanisms in place:

- IKEMEN deterministic game simulation.
- GGPO rollback simulation.
- Existing GGPO session/state logic.
- Existing UDP gameplay transport.
- Existing TCP-derived synchronization path.
- Existing replay file system.
- Existing Lua runtime.
- Existing screenpack/motif architecture.

The goal is to use Nakama to automate coordination rather than replace these systems unnecessarily.

## 4. Replay and Spectator System

### 4.1 Ten-second delayed spectator model

The default spectator publication delay is configured in `src/resources/defaultConfig.ini`:

```ini
Rollback.ReplayBroadcastDelay = 10
```

The replay stream operates at 60 frames per second, so the default delay is:

```text
60 FPS × 10 seconds = 600 frames
```

This delay is substantially larger than IKEMEN GO's rollback correction window and provides a buffer before replay input is exposed to spectators.

### 4.2 Existing replay representation reused

The spectator stream reuses the existing deterministic IKEMEN replay input representation instead of introducing a video stream or a separate game-state replication protocol.

The stream is represented by:

- replay header
- input frame chunks
- replay end message
- replay reset message

Replay chunks are currently 30 frames each.

The existing input representation is reused at eight controller slots and eight bytes of input data per controller per frame.

### 4.3 Rollback-aware publication

`src/replay_stream.go` implements `RollbackReplayStream`.

The rollback session invokes it from the existing replay recording path in `src/rollback.go`.

The stream:

1. Records inputs from the existing rollback timeline.
2. Waits for the configured publication delay.
3. Publishes only input frames that are old enough to be considered settled.
4. Tracks replay truncation.
5. Sends a reset when a rollback reaches data that has already been considered published.

This is intended to prevent speculative rollback data from becoming permanent spectator data.

### 4.4 Spectator-side buffering

`NakamaReplayBuffer` receives and retains replay stream data on the client.

It tracks:

- replay header
- contiguous buffered frame range
- final frame
- reset reason
- individual frame inputs

It can wait for a particular frame to arrive before replay playback consumes it.

### 4.5 Native replay playback

`src/netplay.go` extends `ReplayFile` with a live-buffer mode.

`NewLiveReplayFile()` wraps a `NakamaReplayBuffer` as a `ReplayFile` source.

`ReplayFile.Synchronize()` and `ReplayFile.Update()` recognize live replay buffers and feed the buffered deterministic inputs through the same replay playback path used for ordinary replay files.

The intent is therefore:

```text
Nakama replay chunks
        ↓
NakamaReplayBuffer
        ↓
NewLiveReplayFile
        ↓
existing ReplayFile playback
        ↓
IKEMEN deterministic simulation
```

No separate spectator renderer or video decoder is used.

## 5. Spectator Loading Screen

The ten-second spectator delay is treated as a loading/buffering period instead of showing the underlying game immediately.

The engine tracks the loading state in `System` and waits for the delayed replay buffer to reach the configured delay before beginning replay playback.

The engine-side loading state is exposed through:

```lua
motifState("nakama_spectator_loading")
```

The current implementation also exposes replay status through:

```lua
nakama.replayStatus()
```

including values such as:

- `buffered_through`
- `delay_frames`
- `ready`
- `loading`
- `frame_rate`
- `final_frame`
- `ended`
- `reset_reason`

## 6. Screenpack Integration

Nakama UI that is rendered by the engine is integrated into the motif/screenpack system rather than using fixed engine-only presentation values.

Added to `src/motif.go`:

```text
NakamaInfoProperties
NakamaInfo
NakamaBgDef
```

Added to `src/resources/defaultMotif.ini`:

```ini
[Nakama Info]
[NakamaBgDef]
```

### 6.1 Configurable Nakama loading presentation

The `[Nakama Info]` section provides screenpack-controlled properties for:

- title text
- loading text
- progress text
- remaining-time/detail text
- fonts
- offsets
- scales
- facing
- layers
- windows
- localcoord
- projection parameters
- fade properties
- status text
- error text

Example:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING
```

### 6.2 Configurable background

`[NakamaBgDef]` can provide a dedicated background definition for Nakama screens.

Existing screenpacks remain compatible because the spectator loading text can fall back to the existing `[Title Info]` loading/wait motif when the Nakama-specific element is disabled or unavailable.

### 6.3 Lua-driven screens

Lobby browser, lobby player list, tournament bracket, chat, online status, matchmaking, and similar screens are not hardcoded as a second menu system.

They are intended to be built by the screenpack/game's Lua code from Nakama data and events.

This keeps screenpack design responsibility in the same place as other IKEMEN menus rather than forcing one engine-defined visual style.

## 7. Lua API Added

`src/nakama_script.go` registers a global `nakama` table during `systemScriptInit()`.

Current functions include:

```lua
nakama.connect(config)
nakama.disconnect()
nakama.on(event, callback)

nakama.matchmake(mode, elo, elo_range, game, build, region)
nakama.cancelMatchmaking()

nakama.joinMatch(match_id, token)
nakama.leaveMatch()

nakama.startP2P(port, stun_servers)
nakama.stopP2P()

nakama.replayStatus()
nakama.startSpectatorReplay()

nakama.currentMatch()
nakama.userId()
nakama.username()

nakama.sendMatchData(opcode, data, reliable)
nakama.sendMatchDataBase64(opcode, base64, reliable)

nakama.joinChat(target, channel_type, persistence, hidden)
nakama.sendChat(channel_id, message)

nakama.listLobbies(query, label, min_size, max_size, limit)
nakama.createLobby(config)
nakama.reportLobbyResult(winner_id, loser_id)

nakama.submitResult(match_id, opponent_id, game, result, replay_hash)
```

## 8. Lua Event Model

The Nakama client parses realtime messages off-thread.

Lua callbacks are not invoked directly from the Nakama WebSocket goroutine.

Instead the implementation queues callback work onto IKEMEN's main-thread task mechanism.

The general flow is:

```text
Nakama WebSocket goroutine
        ↓
parse event
        ↓
IKEMEN mainThreadTask queue
        ↓
Lua callback
```

This avoids directly touching the gopher-lua state from a background network goroutine.

The P2P signaling path is also handled separately enough that the P2P handshake does not depend on Lua callbacks running while the game thread is waiting for synchronization.

## 9. Server Modules

### `nakama/modules/ikemen_matchmaker.lua`

Handles:

- ranked/unranked ticket processing
- game/build/region buckets
- server-side Elo lookup
- ranked opponent selection
- creation of `ikemen_session` matches

### `nakama/modules/ikemen_session.lua`

Acts as the Nakama coordination match for a matched 1v1 session.

Handles:

- expected players
- membership
- match data relay
- P2P signaling messages
- replay stream messages

It does not simulate IKEMEN gameplay.

### `nakama/modules/ikemen_lobby.lua`

Handles:

- lobby membership
- maximum eight players
- player ordering
- winner-stays-on scheduling
- round-robin scheduling
- bracket scheduling
- current pairing
- result progression
- lobby state broadcasts

### `nakama/modules/ikemen_rating.lua`

Handles:

- server-side rating storage
- Elo calculation
- reciprocal result validation
- duplicate settlement protection
- optional replay hash agreement

## 10. Code Changes by File

### Added files

| File | Purpose |
|---|---|
| `src/nakama_client.go` | Nakama REST/realtime client, matchmaking, lobby access, chat, signaling, replay transport hooks. |
| `src/nakama_p2p.go` | STUN, candidate gathering, UDP hole punching, P2P connection lifecycle. |
| `src/nakama_script.go` | Lua bindings and Lua event dispatch for Nakama functionality. |
| `src/replay_stream.go` | Rollback-aware delayed replay stream and client-side replay buffer. |
| `nakama/modules/ikemen_matchmaker.lua` | Ranked/unranked Nakama matchmaking processor. |
| `nakama/modules/ikemen_session.lua` | Coordination match for matched players and stream/signaling messages. |
| `nakama/modules/ikemen_lobby.lua` | Authoritative eight-player lobby and tournament scheduler. |
| `nakama/modules/ikemen_rating.lua` | Server-side Elo storage and result settlement. |
| `nakama/examples/ikemen_nakama.lua` | Example Lua integration/event registration. |
| `nakama/README.md` | Integration notes and usage documentation. |

### Modified files

| File | Changes |
|---|---|
| `src/rollback.go` | Connects the rollback session to the delayed replay stream, records frames when replay streaming is enabled, propagates replay truncation, ends the stream at match completion, and initializes the stream from configuration. |
| `src/netplay.go` | Stores synchronized replay metadata and exposes `ReplayFile` live-buffer mode for deterministic spectator playback. |
| `src/system.go` | Adds Nakama client state, spectator loading state, loading-screen rendering, delayed replay startup, and Nakama state rendering. |
| `src/motif.go` | Adds Nakama motif properties and optional Nakama background definition. |
| `src/resources/defaultMotif.ini` | Adds default Nakama screenpack configuration. |
| `src/resources/defaultConfig.ini` | Adds `Rollback.ReplayBroadcastDelay = 10`. |
| `src/script.go` | Registers the Nakama Lua API and exposes Nakama-related motif states. |
| `go.mod` | Pass 5 changed the Go language version from `1.27.0` to `1.23.2` as a local environment workaround. This is not the intended project requirement and should be restored to the project's required Go version for normal builds. |

## 11. Existing Network Transport and Current Integration Boundary

The implementation intentionally did not replace the external GGPO package's transport in the supplied tree.

The current IKEMEN rollback setup still uses the existing GGPO construction path and its existing UDP transport.

The new `NakamaP2P` object can establish and retain a usable UDP socket and exposes:

```text
UDPConn()
RemoteAddr()
```

The intended final direct-connection integration is:

```text
Nakama match
    ↓
P2P signaling
    ↓
STUN / UDP hole punch
    ↓
retained UDP socket
    ↓
existing GGPO transport boundary
    ↓
existing GGPO rollback session
```

The current source has not yet completed the last step of passing the already-established socket directly into the external GGPO transport constructor.

For this reason, the P2P code currently represents a connection-establishment primitive rather than a finished replacement for IKEMEN's existing gameplay transport.

## 12. Result Reporting and Ranked Integrity

The client can submit match results and a replay hash.

The Nakama rating module waits for reciprocal claims and requires:

- matching match identity
- matching opponent identity
- matching game identity
- complementary results
- matching replay hashes when both clients provide hashes

When those conditions are met, Elo is updated for both players.

This prevents simple one-sided accidental result updates and duplicate settlement, but it does not independently prove that the reported replay or result is truthful.

A future authoritative deterministic verifier could run the final replay against a compatible headless IKEMEN build before accepting ranked results. That verifier is not implemented in the current tree.

## 13. Compatibility / Sync Considerations

The existing IKEMEN synchronized replay header carries sync settings and a content fingerprint field.

However, the current engine implementation still contains a `currentContentFingerprint()` TODO and currently returns an empty string.

For production ranked matching, the fingerprint should be made meaningful and should cover all simulation-relevant content/build inputs before treating it as a reliable compatibility gate.

The matchmaking identity already carries game/build information so incompatible client builds can be separated at the Nakama layer.

## 14. Current Limitations

The current Pass 5 tree is an integration prototype rather than a finished production service.

The remaining major work is:

1. **Attach the retained NAT-traversed UDP socket to the external GGPO transport.**
   The current code stops at the transport handoff boundary.

2. **Automatic fallback behavior for networks where direct traversal fails.**
   A TURN/relay fallback has not been implemented.

3. **Fully generic live spectator initialization.**
   The deterministic input buffer and live `ReplayFile` support are present, but arbitrary game-specific replay initialization still depends on the game's setup/Lua path.

4. **New spectator joins.**
   The current documentation/implementation assumes a lobby member can receive the stream from its beginning. Historical replay backfill for a user who joins after the match has already started is not yet a complete protocol.

5. **Authoritative ranked replay verification.**
   Reciprocal claims and replay hashes are not a replacement for deterministic server verification.

6. **Production Nakama deployment configuration.**
   The Lua modules are supplied as integration modules; production deployment still requires configuration of the actual Nakama server, authentication, storage, runtime module loading, and deployment-specific STUN/relay infrastructure.

## 15. Validation Status

The supplied Pass 5 source tree was compared against the original IKEMEN GO source tree supplied for the project.

Pass 5 adds approximately 3,700 lines of new code/configuration and modifies the existing engine in a relatively small number of locations. Most of the new functionality lives in new files or Nakama modules rather than invasive changes to core combat/simulation code.

Static source parsing/gofmt checks were performed on the modified Go sources during development.

A complete engine build was not established against the original project's declared Go requirement in this environment. Pass 5 temporarily changed the `go` directive in `go.mod` to `1.23.2` to match the available local toolchain; this should not be treated as the correct project toolchain requirement. A Go 1.27.1 source archive was subsequently supplied for the environment, but the Pass 5 tree was not rebuilt and repackaged after switching toolchains.

## 16. Design Principle

The implementation is intentionally structured around the smallest useful bridge between Nakama and IKEMEN GO:

```text
Nakama:
    identity
    matchmaking
    ratings
    lobbies
    tournaments
    chat
    signaling
    delayed replay distribution

IKEMEN:
    simulation
    GGPO rollback
    UDP gameplay
    replay playback
    Lua
    screenpacks
```

The objective is to add online-service functionality without creating a second fighting-game networking stack, a second replay system, or a second screen/menu framework when the existing IKEMEN systems can perform the required job.


---

# Pass 6

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass6.md`

# IKEMEN GO + Nakama Matchmaking Integration

## 1. Overview

This project adds a Nakama-backed online service layer to IKEMEN GO while retaining IKEMEN GO's existing deterministic simulation, GGPO rollback system, UDP gameplay transport, replay system, Lua scripting model, and screenpack/motif system as the primary game-side machinery.

The intended architecture is:

```text
                         NAKAMA
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   Authentication      Matchmaking         Lobbies
        │                  │                  │
        │             Ranked/Unranked     Tournament
        │                  │              Scheduling
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │
                     Match/session
                           │
                 P2P signaling / setup
                           │
              ┌────────────┴────────────┐
              │                         │
           Player A                  Player B
              │                         │
              └──── existing GGPO ─────┘
                           │
                   deterministic replay
                           │
                     600-frame delay
                           │
                           ▼
                        Nakama
                           │
                 delayed input stream
                           │
                   ┌───────┼───────┐
                   ▼       ▼       ▼
               spectator spectator spectator
                   │       │       │
                   └── IKEMEN deterministic playback ──┘
```

Nakama is intended to be the control plane and social/matchmaking service. It is not intended to become the live fighting-game simulation server.

## 2. Implemented Features

### 2.1 Nakama client integration

Added an engine-side Nakama client in `src/nakama_client.go`.

Implemented capabilities include:

- REST authentication using device/session credentials.
- Realtime WebSocket connection.
- WebSocket read/write loops.
- Event dispatch from Nakama into IKEMEN.
- Match join/leave.
- Match data send operations.
- Server RPC calls.
- Lobby listing through Nakama authoritative match listing.
- Chat channel join/send.
- P2P signaling messages.
- Replay header/chunk/end/reset messages.

The client is exposed to Lua through `src/nakama_script.go`.

### 2.2 Ranked matchmaking

The Nakama server module `nakama/modules/ikemen_matchmaker.lua` implements ranked and unranked matchmaking.

Ranked matchmaking:

- Groups tickets by game, build, mode, and region.
- Retrieves the player's rating from server-side storage.
- Compares rating distance against the configured allowed range.
- Chooses a compatible opponent.
- Creates a Nakama `ikemen_session` match for the two players.

The client can provide an Elo range as matchmaking metadata, but the authoritative rating is intended to come from server-side storage.

### 2.3 Unranked matchmaking

Unranked matchmaking uses the same matchmaker infrastructure while ignoring Elo distance when pairing compatible tickets.

The matchmaking target is two players.

### 2.4 Server-side Elo

Added `nakama/modules/ikemen_rating.lua`.

The module provides:

- Per-game rating storage.
- Configurable initial rating.
- Configurable K-factor.
- Configurable rating scale.
- Wins/losses/game counters.
- Reciprocal result submission.
- Match-specific settlement protection so the same match is not intended to change ratings twice.
- Optional replay hash agreement checking.

Default configuration currently is:

```lua
initial_rating = 1000
k_factor = 32
rating_scale = 400
```

Game-specific values can be placed in the `GAME_CONFIG` table.

This is not a cryptographic anti-cheat system. It verifies reciprocal result claims, but two cooperating clients could still submit a false matching result unless a separate authoritative replay verification system is added.

### 2.5 Authoritative 8-player lobbies

Added `nakama/modules/ikemen_lobby.lua`.

A lobby supports up to eight players and keeps authoritative state for:

- Player membership/order.
- Lobby name.
- Game/build identity.
- Lobby format.
- Rules metadata.
- Current pairing.
- Tournament state.
- Result progression.

Supported scheduling formats are:

```text
winner_stays_on
round_robin
bracket
```

The lobby coordinates pairings but does not simulate the fight.

### 2.6 Lobby browser

The Lua API exposes `nakama.listLobbies()` through Nakama's match listing API.

The lobby browser is intentionally not implemented as a separate hardcoded renderer. Screenpacks/game scripts can use the returned lobby data to construct their own interface using normal IKEMEN Lua/UI facilities.

### 2.7 Nakama chat

Chat access is provided through the Nakama client:

```lua
nakama.joinChat(target)
nakama.sendChat(channel_id, message)
```

Incoming chat events are exposed to Lua through the Nakama event callback system.

### 2.8 P2P / NAT traversal primitive

Added `src/nakama_p2p.go`.

The current implementation provides:

- Local IPv4 candidate discovery.
- STUN Binding Request support.
- Server-reflexive address discovery.
- UDP hole-punching.
- Shared handshake token validation.
- Persistent UDP socket retention after the P2P connection succeeds.

The P2P object exposes the established UDP connection and remote endpoint so it can eventually be attached to the GGPO transport without closing/rebinding the NAT mapping.

Lua access is currently exposed as:

```lua
nakama.startP2P(port, stun_servers)
nakama.stopP2P()
```

The player is not expected to manually configure the connection method in the intended final flow.

## 3. Existing IKEMEN/GGPO Systems Retained

The implementation deliberately leaves the following existing mechanisms in place:

- IKEMEN deterministic game simulation.
- GGPO rollback simulation.
- Existing GGPO session/state logic.
- Existing UDP gameplay transport.
- Existing TCP-derived synchronization path.
- Existing replay file system.
- Existing Lua runtime.
- Existing screenpack/motif architecture.

The goal is to use Nakama to automate coordination rather than replace these systems unnecessarily.

## 4. Replay and Spectator System

### 4.1 Ten-second delayed spectator model

The default spectator publication delay is configured in `src/resources/defaultConfig.ini`:

```ini
Rollback.ReplayBroadcastDelay = 10
```

The replay stream operates at 60 frames per second, so the default delay is:

```text
60 FPS × 10 seconds = 600 frames
```

This delay is substantially larger than IKEMEN GO's rollback correction window and provides a buffer before replay input is exposed to spectators.

### 4.2 Existing replay representation reused

The spectator stream reuses the existing deterministic IKEMEN replay input representation instead of introducing a video stream or a separate game-state replication protocol.

The stream is represented by:

- replay header
- input frame chunks
- replay end message
- replay reset message

Replay chunks are currently 30 frames each.

The existing input representation is reused at eight controller slots and eight bytes of input data per controller per frame.

### 4.3 Rollback-aware publication

`src/replay_stream.go` implements `RollbackReplayStream`.

The rollback session invokes it from the existing replay recording path in `src/rollback.go`.

The stream:

1. Records inputs from the existing rollback timeline.
2. Waits for the configured publication delay.
3. Publishes only input frames that are old enough to be considered settled.
4. Tracks replay truncation.
5. Sends a reset when a rollback reaches data that has already been considered published.

This is intended to prevent speculative rollback data from becoming permanent spectator data.

### 4.4 Spectator-side buffering

`NakamaReplayBuffer` receives and retains replay stream data on the client.

It tracks:

- replay header
- contiguous buffered frame range
- final frame
- reset reason
- individual frame inputs

It can wait for a particular frame to arrive before replay playback consumes it.

### 4.5 Native replay playback

`src/netplay.go` extends `ReplayFile` with a live-buffer mode.

`NewLiveReplayFile()` wraps a `NakamaReplayBuffer` as a `ReplayFile` source.

`ReplayFile.Synchronize()` and `ReplayFile.Update()` recognize live replay buffers and feed the buffered deterministic inputs through the same replay playback path used for ordinary replay files.

The intent is therefore:

```text
Nakama replay chunks
        ↓
NakamaReplayBuffer
        ↓
NewLiveReplayFile
        ↓
existing ReplayFile playback
        ↓
IKEMEN deterministic simulation
```

No separate spectator renderer or video decoder is used.

## 5. Spectator Loading Screen

The ten-second spectator delay is treated as a loading/buffering period instead of showing the underlying game immediately.

The engine tracks the loading state in `System` and waits for the delayed replay buffer to reach the configured delay before beginning replay playback.

The engine-side loading state is exposed through:

```lua
motifState("nakama_spectator_loading")
```

The current implementation also exposes replay status through:

```lua
nakama.replayStatus()
```

including values such as:

- `buffered_through`
- `delay_frames`
- `ready`
- `loading`
- `frame_rate`
- `final_frame`
- `ended`
- `reset_reason`

## 6. Screenpack Integration

Nakama UI that is rendered by the engine is integrated into the motif/screenpack system rather than using fixed engine-only presentation values.

Added to `src/motif.go`:

```text
NakamaInfoProperties
NakamaInfo
NakamaBgDef
```

Added to `src/resources/defaultMotif.ini`:

```ini
[Nakama Info]
[NakamaBgDef]
```

### 6.1 Configurable Nakama loading presentation

The `[Nakama Info]` section provides screenpack-controlled properties for:

- title text
- loading text
- progress text
- remaining-time/detail text
- fonts
- offsets
- scales
- facing
- layers
- windows
- localcoord
- projection parameters
- fade properties
- status text
- error text

Example:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING
```

### 6.2 Configurable background

`[NakamaBgDef]` can provide a dedicated background definition for Nakama screens.

Existing screenpacks remain compatible because the spectator loading text can fall back to the existing `[Title Info]` loading/wait motif when the Nakama-specific element is disabled or unavailable.

### 6.3 Lua-driven screens

Lobby browser, lobby player list, tournament bracket, chat, online status, matchmaking, and similar screens are not hardcoded as a second menu system.

They are intended to be built by the screenpack/game's Lua code from Nakama data and events.

This keeps screenpack design responsibility in the same place as other IKEMEN menus rather than forcing one engine-defined visual style.

## 7. Lua API Added

`src/nakama_script.go` registers a global `nakama` table during `systemScriptInit()`.

Current functions include:

```lua
nakama.connect(config)
nakama.disconnect()
nakama.on(event, callback)

nakama.matchmake(mode, elo, elo_range, game, build, region)
nakama.cancelMatchmaking()

nakama.joinMatch(match_id, token)
nakama.leaveMatch()

nakama.startP2P(port, stun_servers)
nakama.stopP2P()

nakama.replayStatus()
nakama.startSpectatorReplay()

nakama.currentMatch()
nakama.userId()
nakama.username()

nakama.sendMatchData(opcode, data, reliable)
nakama.sendMatchDataBase64(opcode, base64, reliable)

nakama.joinChat(target, channel_type, persistence, hidden)
nakama.sendChat(channel_id, message)

nakama.listLobbies(query, label, min_size, max_size, limit)
nakama.createLobby(config)
nakama.reportLobbyResult(winner_id, loser_id)

nakama.submitResult(match_id, opponent_id, game, result, replay_hash)
```

## 8. Lua Event Model

The Nakama client parses realtime messages off-thread.

Lua callbacks are not invoked directly from the Nakama WebSocket goroutine.

Instead the implementation queues callback work onto IKEMEN's main-thread task mechanism.

The general flow is:

```text
Nakama WebSocket goroutine
        ↓
parse event
        ↓
IKEMEN mainThreadTask queue
        ↓
Lua callback
```

This avoids directly touching the gopher-lua state from a background network goroutine.

The P2P signaling path is also handled separately enough that the P2P handshake does not depend on Lua callbacks running while the game thread is waiting for synchronization.

## 9. Server Modules

### `nakama/modules/ikemen_matchmaker.lua`

Handles:

- ranked/unranked ticket processing
- game/build/region buckets
- server-side Elo lookup
- ranked opponent selection
- creation of `ikemen_session` matches

### `nakama/modules/ikemen_session.lua`

Acts as the Nakama coordination match for a matched 1v1 session.

Handles:

- expected players
- membership
- match data relay
- P2P signaling messages
- replay stream messages

It does not simulate IKEMEN gameplay.

### `nakama/modules/ikemen_lobby.lua`

Handles:

- lobby membership
- maximum eight players
- player ordering
- winner-stays-on scheduling
- round-robin scheduling
- bracket scheduling
- current pairing
- result progression
- lobby state broadcasts

### `nakama/modules/ikemen_rating.lua`

Handles:

- server-side rating storage
- Elo calculation
- reciprocal result validation
- duplicate settlement protection
- optional replay hash agreement

## 10. Code Changes by File

### Added files

| File | Purpose |
|---|---|
| `src/nakama_client.go` | Nakama REST/realtime client, matchmaking, lobby access, chat, signaling, replay transport hooks. |
| `src/nakama_p2p.go` | STUN, candidate gathering, UDP hole punching, P2P connection lifecycle. |
| `src/nakama_script.go` | Lua bindings and Lua event dispatch for Nakama functionality. |
| `src/replay_stream.go` | Rollback-aware delayed replay stream and client-side replay buffer. |
| `nakama/modules/ikemen_matchmaker.lua` | Ranked/unranked Nakama matchmaking processor. |
| `nakama/modules/ikemen_session.lua` | Coordination match for matched players and stream/signaling messages. |
| `nakama/modules/ikemen_lobby.lua` | Authoritative eight-player lobby and tournament scheduler. |
| `nakama/modules/ikemen_rating.lua` | Server-side Elo storage and result settlement. |
| `nakama/examples/ikemen_nakama.lua` | Example Lua integration/event registration. |
| `nakama/README.md` | Integration notes and usage documentation. |

### Modified files

| File | Changes |
|---|---|
| `src/rollback.go` | Connects the rollback session to the delayed replay stream, records frames when replay streaming is enabled, propagates replay truncation, ends the stream at match completion, and initializes the stream from configuration. |
| `src/netplay.go` | Stores synchronized replay metadata and exposes `ReplayFile` live-buffer mode for deterministic spectator playback. |
| `src/system.go` | Adds Nakama client state, spectator loading state, loading-screen rendering, delayed replay startup, and Nakama state rendering. |
| `src/motif.go` | Adds Nakama motif properties and optional Nakama background definition. |
| `src/resources/defaultMotif.ini` | Adds default Nakama screenpack configuration. |
| `src/resources/defaultConfig.ini` | Adds `Rollback.ReplayBroadcastDelay = 10`. |
| `src/script.go` | Registers the Nakama Lua API and exposes Nakama-related motif states. |
| `go.mod` | Pass 5 changed the Go language version from `1.27.0` to `1.23.2` as a local environment workaround. This is not the intended project requirement and should be restored to the project's required Go version for normal builds. |

## 11. Existing Network Transport and Current Integration Boundary

The implementation intentionally did not replace the external GGPO package's transport in the supplied tree.

The current IKEMEN rollback setup still uses the existing GGPO construction path and its existing UDP transport.

The new `NakamaP2P` object can establish and retain a usable UDP socket and exposes:

```text
UDPConn()
RemoteAddr()
```

The intended final direct-connection integration is:

```text
Nakama match
    ↓
P2P signaling
    ↓
STUN / UDP hole punch
    ↓
retained UDP socket
    ↓
existing GGPO transport boundary
    ↓
existing GGPO rollback session
```

The current source has not yet completed the last step of passing the already-established socket directly into the external GGPO transport constructor.

For this reason, the P2P code currently represents a connection-establishment primitive rather than a finished replacement for IKEMEN's existing gameplay transport.

## 12. Result Reporting and Ranked Integrity

The client can submit match results and a replay hash.

The Nakama rating module waits for reciprocal claims and requires:

- matching match identity
- matching opponent identity
- matching game identity
- complementary results
- matching replay hashes when both clients provide hashes

When those conditions are met, Elo is updated for both players.

This prevents simple one-sided accidental result updates and duplicate settlement, but it does not independently prove that the reported replay or result is truthful.

A future authoritative deterministic verifier could run the final replay against a compatible headless IKEMEN build before accepting ranked results. That verifier is not implemented in the current tree.

## 13. Compatibility / Sync Considerations

The existing IKEMEN synchronized replay header carries sync settings and a content fingerprint field.

However, the current engine implementation still contains a `currentContentFingerprint()` TODO and currently returns an empty string.

For production ranked matching, the fingerprint should be made meaningful and should cover all simulation-relevant content/build inputs before treating it as a reliable compatibility gate.

The matchmaking identity already carries game/build information so incompatible client builds can be separated at the Nakama layer.

## 14. Current Limitations

The current Pass 5 tree is an integration prototype rather than a finished production service.

The remaining major work is:

1. **Attach the retained NAT-traversed UDP socket to the external GGPO transport.**
   The current code stops at the transport handoff boundary.

2. **Automatic fallback behavior for networks where direct traversal fails.**
   A TURN/relay fallback has not been implemented.

3. **Fully generic live spectator initialization.**
   The deterministic input buffer and live `ReplayFile` support are present, but arbitrary game-specific replay initialization still depends on the game's setup/Lua path.

4. **New spectator joins.**
   The current documentation/implementation assumes a lobby member can receive the stream from its beginning. Historical replay backfill for a user who joins after the match has already started is not yet a complete protocol.

5. **Authoritative ranked replay verification.**
   Reciprocal claims and replay hashes are not a replacement for deterministic server verification.

6. **Production Nakama deployment configuration.**
   The Lua modules are supplied as integration modules; production deployment still requires configuration of the actual Nakama server, authentication, storage, runtime module loading, and deployment-specific STUN/relay infrastructure.

## 15. Validation Status

The supplied Pass 5 source tree was compared against the original IKEMEN GO source tree supplied for the project.

Pass 5 adds approximately 3,700 lines of new code/configuration and modifies the existing engine in a relatively small number of locations. Most of the new functionality lives in new files or Nakama modules rather than invasive changes to core combat/simulation code.

Static source parsing/gofmt checks were performed on the modified Go sources during development.

A complete engine build was not established against the original project's declared Go requirement in this environment. Pass 5 temporarily changed the `go` directive in `go.mod` to `1.23.2` to match the available local toolchain; this should not be treated as the correct project toolchain requirement. A Go 1.27.1 source archive was subsequently supplied for the environment, but the Pass 5 tree was not rebuilt and repackaged after switching toolchains.

## 16. Design Principle

The implementation is intentionally structured around the smallest useful bridge between Nakama and IKEMEN GO:

```text
Nakama:
    identity
    matchmaking
    ratings
    lobbies
    tournaments
    chat
    signaling
    delayed replay distribution

IKEMEN:
    simulation
    GGPO rollback
    UDP gameplay
    replay playback
    Lua
    screenpacks
```

The objective is to add online-service functionality without creating a second fighting-game networking stack, a second replay system, or a second screen/menu framework when the existing IKEMEN systems can perform the required job.


## Pass 6 continuation — recovered work and current delta

Pass 6 continues directly from the supplied Pass 5 tree. It does not replace the existing IKEMEN/GGPO simulation architecture.

### Recovered Pass 5 work preserved

The supplied Pass 5 tree already contains the Nakama client, ranked/unranked matchmaking, server-side Elo module, authoritative lobbies, chat, P2P/STUN/hole-punching, delayed replay streaming, spectator loading presentation, Lua bindings, replay synchronization, and the synchronization/rollback changes described above. Those systems remain the baseline.

### Pass 6 changes

1. `NakamaReplayBuffer.ApplyChunk` now advances the contiguous-frame boundary after every chunk. Previously the advancement path was limited to an initial-frame case, which could leave `BufferedThrough` stalled after the first chunk.

2. `NakamaReplayBuffer.Reset` now clears the received replay header as well as frames and end-state data. A reset therefore requires a new replay header before spectator startup can proceed.

3. The Nakama client can transfer ownership of an established `NakamaP2P` UDP socket through `TakeP2PTransport`. The transfer removes the socket from the P2P object's ownership before it is closed.

4. Rollback startup now prefers the retained Nakama UDP socket when the client is a member of a Nakama match and P2P is ready. It verifies that the P2P socket uses the configured `Rollback.Port`, obtains the actual observed remote UDP endpoint, and passes the retained socket to the GGPO transport.

5. `RollbackSession.InitP1` and `InitP2` accept an optional pre-bound UDP socket. Legacy GGPO socket creation remains the fallback when no pre-bound socket is supplied.

6. The spectator loading renderer no longer resets animated text every frame. Text sprites are updated once per frame, and Nakama-specific loading text falls back to the established title loading/wait text when the Nakama text is empty.

7. Strict synchronization validation now rejects a strict-setting schema length/path mismatch before value comparison. This closes the missing-field compatibility hole in the earlier strict comparison.

8. Realtime Lua callbacks are explicitly marshalled onto the engine main-thread task queue instead of invoking gopher-lua from a network goroutine. The queue is best-effort and a saturated queue drops the event rather than blocking the networking goroutine. Callback storage is local to the Lua setup instead of being held in a package-global map.

9. `JoinMatchWithMetadata` supplies the configured game/build identity when the caller does not provide them and no longer mutates the caller's metadata map.

10. Nakama Lua server modules now explicitly import `require("nakama")`, and `nakama/modules/main.lua` registers the authoritative `ikemen_session` and `ikemen_lobby` handlers while loading the matchmaker and rating modules.

11. Lobby join validation now requires game/build identity to match the lobby whenever those lobby identities are populated.

12. The lobby bracket scheduler now consumes automatic byes and advances to the next real match instead of leaving a `player2=nil` pairing active.

13. Nakama `dispatcher.broadcast_message` calls were normalized to the documented four-argument form.

14. `RollbackReplayStream.End` now flushes all remaining authoritative replay frames before publishing the final-frame marker. This closes a short-match/early-end case where the normal ten-second publication window had not elapsed before the fight ended.

15. `currentContentFingerprint()` now produces a deterministic SHA-256 digest from the engine version, active motif/fight-screen definitions, loaded character definitions and character FightFX, the active stage and attached-character definitions, and loaded common FightFX. The digest uses role labels and file contents rather than absolute installation paths.

### External GGPO boundary

The Pass 6 source now contains the IKEMEN-side transport handoff. The remaining dependency-side change is intentionally small: the pinned `github.com/ikemen-engine/ggpo` transport package must expose a constructor that wraps an already-open `net.PacketConn` without binding a second UDP socket. The implementation requirement is documented in `patches/ggpo-nakama-transport.md`.

The dependency is not vendored in the supplied repository and could not be fetched in this environment, so the constructor itself has not been compiled against the exact pinned GGPO source here.

### Ranked-result integrity correction

The earlier Pass 5 description of duplicate settlement protection was stronger than the implementation supports. The current Elo module stores a settlement marker per submitting user. Two reciprocal RPCs can race before either user's marker exists, so the marker is not an atomic match-wide claim. The implementation should therefore be treated as reciprocal-claim reconciliation, not complete duplicate-settlement protection, until settlement is moved into a single authoritative match path or a verified atomic compare-and-swap/transactional storage mechanism is used.

### Validation state

The project `go.mod` is restored to `go 1.27.0`. The available local compiler is Go 1.23.2, and the exact pinned GGPO source is not present in the local module cache. A full engine test was attempted with `GOTOOLCHAIN=local go test ./src` and is blocked immediately because the local toolchain is below the module's declared minimum. Consequently a full engine build has not been claimed.

Focused validation completed successfully:

- standalone P2P ownership tests: `go test ./...` → pass
- standalone replay-buffer/replay-stream tests, including final-frame flushing: `go test ./...` → pass
- Lua syntax checks for all Nakama modules and the example: all pass

### Recovered continuation state

This pass was reconstructed from the supplied Pass 5 source tree plus the later implementation material available in the project. The source tree, rather than an unavailable prior-chat transcript, is the authoritative record for code that is claimed as implemented here.

The next dependency-side blocker remains the pinned GGPO transport adapter. The content fingerprint is now meaningful and hashes the engine version plus active match definitions and loaded FightFX, but it is intentionally not claimed as an exhaustive digest of every dynamically loaded asset or runtime Lua file.


---

# Pass 7

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass7.md`

# IKEMEN GO + Nakama Matchmaking Integration — Pass 7

## 1. Current architecture

Nakama supplies identity, matchmaking, ratings, lobbies, tournaments, chat,
signaling, and delayed replay distribution. IKEMEN GO remains responsible for
deterministic simulation, GGPO rollback, gameplay transport, replay playback,
Lua scripting, and screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket
is transferred into GGPO without rebinding so that the NAT traversal state is
preserved.

## 2. Recovered Pass 5/6 implementation

The current tree retains the previously completed work for:

- synchronized configuration schemas and session overrides;
- rollback UDP-port negotiation and compatibility checks;
- replay synchronization headers and restoration;
- content/build fingerprinting;
- Nakama client, matchmaking, lobby, chat and result APIs;
- Nakama Lua server modules and `modules/main.lua` registration;
- Lua-thread-safe event dispatch;
- STUN candidate gathering and UDP hole punching;
- delayed spectator replay buffering and loading presentation;
- replay final-frame flushing;
- Lua bindings for Nakama functionality.

## 3. Pass 7 changes

### 3.1 Bundled GGPO transport adapter

The exact pinned GGPO source is bundled under `third_party/ggpo` and the root
`go.mod` replaces the remote module with that local copy for reproducible builds.

Added:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

The constructor uses the existing GGPO `Udp` implementation and sender/receiver
lifecycle while adopting an already-bound `net.PacketConn`. It does not create a
second socket. The existing `NewUdp` constructor delegates to the same internal
initializer after binding its own socket.

### 3.2 Nakama P2P self-signal protection

Nakama match broadcasts can be delivered to the publishing client. The P2P
signal handler now ignores a signal whose token is the local token instead of
adopting the local socket as the remote peer.

### 3.3 Focused P2P regression coverage

Added engine-side tests for:

- ignoring self-originated P2P signals;
- completing a two-peer localhost UDP handshake;
- recording the actual observed remote endpoint for each peer.

## 4. GGPO verification

The bundled GGPO transport package passes its lifecycle tests, including the new
`NewUdpFromPacketConn` test covering socket reuse, unchanged local port, packet
receipt, close/unblock, and subsequent port rebind.

The GGPO core packages compile in an isolated validation copy. Existing upstream
GGPO tests unrelated to the transport adapter have pre-existing behavioral
failures in that validation environment; those failures are not represented as
transport-adapter failures.

## 5. Remaining validation boundary

A complete IKEMEN engine build still requires an actual Go 1.27.x compiler and the
project's native/build dependencies. The supplied Go 1.27.1 source archive is not
itself a compiled bootstrap toolchain, and the environment lacks the required
Go 1.24.6-or-newer bootstrap compiler.

The next real acceptance test is an end-to-end two-client match using a real
Nakama server and the bundled GGPO transport:

```text
Nakama authentication
    -> matchmaker
    -> ikemen_session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO transport
    -> rollback fight
    -> replay publication
```

## 6. Known limitations

TURN/relay fallback is not implemented. Late-joining spectator backfill is not
a complete protocol. Ranked-result handling still uses reciprocal claims and is
not an authoritative deterministic replay verifier. The content fingerprint
covers the configured active match/build definitions and loaded FightFX assets,
but it is not claimed to be an exhaustive digest of every dynamically loaded
runtime asset or Lua module.

The Elo settlement path remains non-atomic: two reciprocal submissions can race
before a match-wide settlement marker exists. It should therefore not be treated
as complete duplicate-settlement protection for a public ranked service.

## 7. Build integration

The development tree contains the GGPO source at `third_party/ggpo` and uses:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The standalone dependency patch is also supplied at:

```text
patches/ggpo-nakama-transport.patch
```

so the same change can be applied to the upstream GGPO module instead of using
the bundled source when the dependency is maintained externally.


---

# Pass 8

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass8.md`

# IKEMEN GO + Nakama — Consolidated Implementation Record, Pass 8

## 1. Record scope

This file is the consolidated implementation record for the current development tree. It supersedes the separate Pass 6 and Pass 7 summaries for status tracking while preserving their detailed historical material in the appendices. The source tree is authoritative for what is actually present in code.

## 2. Current architecture

Nakama provides account identity, matchmaking, lobby/presence, chat, signaling, ratings/result plumbing, and delayed replay distribution. IKEMEN GO remains responsible for deterministic simulation, GGPO rollback, gameplay transport, replay playback, Lua execution, renderer composition, and game/screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket is transferred into GGPO without rebinding so that the NAT traversal state is preserved.

The optional border/presentation system is render-only. It provides a physical outer canvas, independently configured game viewport, border-authored assets, and Lua render hooks. It does not participate in rollback state or deterministic simulation.

Ranked sets are session/presentation state. Best-of-X, side switching, and winner-selection locking are negotiated outside rollback and propagated through Nakama matchmaking/session metadata.

## 3. Completed implementation through Pass 8

### 3.1 Synchronization and session overrides

The tree contains synchronized configuration schemas, strict/host scopes, deterministic serialization/validation, session-wide overrides, restoration, synchronization warnings, effective fight-aspect compatibility checks, replay synchronization headers, rollback UDP-port negotiation, and content/build fingerprint plumbing.

### 3.2 Nakama integration

The tree contains the Nakama client, matchmaking, lobby/presence, chat, result plumbing, server Lua modules, `modules/main.lua` registration, Lua-thread-safe event dispatch, STUN candidate gathering, UDP hole punching, P2P signaling, delayed spectator replay loading, and Lua bindings for Nakama/session information.

Nakama match broadcasting is protected against accepting the local client's own P2P signal.

### 3.3 GGPO transport integration

The pinned GGPO source is bundled in `third_party/ggpo` and the root module uses a local replacement:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The GGPO transport exposes:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

This adopts the already-bound Nakama/STUN `PacketConn` without creating a second UDP socket. The existing GGPO constructor delegates to the same initializer after binding its own socket.

The transport lifecycle test verifies socket reuse, unchanged local port, packet receipt, close/unblock, and subsequent port rebind.

### 3.4 Presentation canvas / border system

The engine now distinguishes the physical presentation canvas from the game viewport.

When border presentation is enabled:

```text
outer canvas = Video.Border.Width  × Video.Border.Height
game viewport = Video.GameWidth     × Video.GameHeight
```

The game viewport is centered inside the border by default. Signed `Video.Border.GameOffsetX` and `Video.Border.GameOffsetY` move it relative to that centered position.

Command-line controls:

```text
-bordered
-borderwidth <pixels>
-borderheight <pixels>
-bordergamex <pixels>
-bordergamey <pixels>
```

Normal non-bordered resolutions continue to use the game resolution as the output resolution. Bordered resolution presets bind an outer resolution to an independent game resolution. Current defaults include:

```text
1280x720  -> 960x720
1600x900  -> 1200x900
1920x1080 -> 1440x1080
1920x1200 -> 1600x1200
2560x1440 -> 1920x1440
```

The options system has separate `Resolution` and `Bordered Resolutions` menus. Bordered presets enable presentation and reset placement offsets. Custom bordered resolution accepts outer width/height and game width/height independently.

The renderer paths were updated for OpenGL 3.3, OpenGL ES 3.2, and Vulkan. Font rendering uses the active presentation viewport. Scissor rectangles account for the presentation origin.

### 3.5 Border authoring

The motif now contains a `[Border]` section with separate background and foreground layers. Each layer uses the engine's existing animation, text, and rectangle primitives.

The intended syntax is:

```text
[Border]
enabled = 1
localcoord = 1920, 1080
background.anim = ...
background.text.text = ...
background.overlay.col = ...
background.overlay.alpha = ...
foreground.anim = ...
foreground.text.text = ...
foreground.overlay.col = ...
foreground.overlay.alpha = ...
```

`BorderLayerProperties` uses the existing `AnimationProperties` directly plus grouped `text` and `overlay` sections. The border's own `localcoord` is the inherited coordinate space for child animation/text/overlay data unless a child explicitly supplies its own localcoord.

The border layer updates animation, text, and rectangle state each render frame and renders across the existing layer numbers. Render hooks are:

```text
game.border_bg
game.border
```

They execute on the outer presentation canvas. Existing Lua animation, text, and rectangle APIs can therefore be used to construct dynamic side panels, player information, chat, spectator data, challenge displays, and other UI without coupling those elements to the rollback simulation.

Lua accessors include:

```text
borderEnabled()
borderWidth()
borderHeight()
borderGameX()
borderGameY()
borderGameWidth()
borderGameHeight()
```

The border is deliberately not implemented as a conventional OS window border. It is a composition surface around the gameplay viewport so game and border resolutions remain independently addressable.

### 3.6 Ranked best-of-X sets

`Netplay.Ranked` contains:

```text
BestOf
SwitchSides
WinnerKeepsSelection
```

`BestOf` is normalized to an odd value from 1 through 99.

The matchmaker includes the negotiated ranked rules in its compatibility bucket and session label so players with different set rules are not silently paired into one rule set.

A render/session-only `RankedSet` object tracks:

- stable player identities;
- current physical side assignment;
- wins and completed match count;
- set winner;
- immediate previous match winner;
- character/team selection tokens.

A set completes as soon as one player reaches the majority required by BestOf. When `SwitchSides` is enabled, physical side assignment swaps only after a non-terminal match. Local controller mappings are then swapped so player identity follows the assigned side.

When `WinnerKeepsSelection` is enabled, the previous match winner's confirmed character/team selection is carried into the next match after the side assignment is resolved. The loser remains selectable.

The selection carry is applied before the next team selection menu is constructed, and the finished set exits the repeated-match loop when a majority winner is reached.

### 3.7 Ranked Lua/session API

The Nakama Lua surface exposes ranked-set state and actions through:

```text
nakama.rankedSet()
nakama.rankedSetRecordMatch(winnerSide)
nakama.rankedSetSelectionLocked(player, token)
```

The state table includes set activity, BestOf, match count, win counts, player IDs, physical side mapping, switch-side policy, winner-selection policy, set winner, and previous match winner.

### 3.8 Replay and spectator behavior

Delayed replay distribution remains integrated with rollback. The replay buffer flushes final delayed frames before the live stream is terminated, including matches shorter than the normal spectator delay.

Spectator replay startup, loading UI, replay synchronization headers, and session configuration restoration remain part of the existing implementation.

### 3.9 Content fingerprint

The content fingerprint now produces a deterministic SHA-256 over the configured active match/build definitions and loaded FightFX assets. Machine-specific absolute paths are not used as the hash identity.

The implementation is intentionally not described as an exhaustive digest of every dynamically loaded runtime asset or Lua module; that remains a separate hardening task.

## 4. Corrective fixes made during Pass 8 continuation

### 4.1 Ranked-set reset state

`RankedSet.Reset()` now explicitly restores both `winner` and `lastWinner` to `-1`. This prevents a reset ranked session from exposing a stale previous-winner state through Lua.

A regression test covers the reset behavior.

### 4.2 Border motif field layout

The border animation definition is flattened to use the same convention as other motif animation properties (`background.anim`, etc.) while keeping text and overlay grouped under their respective keys.

### 4.3 Border localcoord inheritance

The data-pointer population path recognizes `BorderInfoProperties.Localcoord` as the coordinate-space root for border children. Explicit per-element localcoord values remain authoritative.

### 4.4 Border rectangle animation

Border overlay rectangles are updated every render frame so pulsing alpha and related rectangle state animate correctly.

### 4.5 Lua hook syntax correction

The presentation-hook documentation comment was corrected so it does not become part of a malformed Lua function declaration.

## 5. Validation performed

### Passed

- ranked-set standalone tests using the available Go 1.23 compiler:
  - BestOf normalization;
  - majority set completion;
  - side switching;
  - winner-selection lock;
  - reset clears previous winner.
- P2P self-signal rejection test.
- Two-peer localhost UDP punching/handshake test.
- GGPO `NewUdpFromPacketConn` lifecycle test.
- GGPO core package compilation in an isolated validation copy.
- Replay-buffer final-frame flush regression test.
- Source formatting with `gofmt` for modified Go files.

### Not yet fully validated

A complete IKEMEN engine build/test run has not been completed in this environment because the project requires Go 1.27.0+ and the local compiler is Go 1.23.2. The supplied Go 1.27.1 archive is source code and itself requires a Go 1.24.6-or-newer bootstrap compiler.

The environment also has no network access for automatic module/toolchain acquisition and no installed Lua interpreter for an independent Lua parser/runtime check.

## 6. Build status

The source tree is structured for the project's existing platform build scripts. The bundled GGPO dependency is part of the source tree and does not require a separately maintained fork at build time.

Builds still require the appropriate target toolchain and native dependencies described by `BUILDING.md` for Windows, Linux, macOS, and Android.

The current tree should therefore be treated as a development/test release candidate rather than as a fully cross-compiled and end-to-end validated public release.

## 7. Remaining engineering work

The major remaining online acceptance boundary is a real two-client session:

```text
Nakama authentication
    -> matchmaking
    -> session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO
    -> real rollback fight
    -> replay publication
```

Known limitations remain:

- TURN/relay fallback is not implemented;
- late-joining spectator backfill is incomplete;
- ranked result handling is not yet an authoritative deterministic replay verifier;
- Elo/result settlement is not atomic duplicate-settlement protection;
- content fingerprinting is not exhaustive of every dynamic runtime resource.

## 8. Pass 8 design rules

The border feature must remain presentation-only. Dynamic border information may include player data, online status, chat, spectators, lobby state, challenge state, and other non-deterministic UI information, but it must not directly mutate rollback gameplay state during a synchronized fight.

Border presentation should remain disabled without opt-in and must preserve ordinary non-bordered behavior when disabled.

Ranked set rules are session state, not rollback state. The game simulation remains governed by the existing deterministic synchronization and GGPO mechanisms.

## Appendix A — Pass 6 historical record


## 1. Overview

This project adds a Nakama-backed online service layer to IKEMEN GO while retaining IKEMEN GO's existing deterministic simulation, GGPO rollback system, UDP gameplay transport, replay system, Lua scripting model, and screenpack/motif system as the primary game-side machinery.

The intended architecture is:

```text
                         NAKAMA
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   Authentication      Matchmaking         Lobbies
        │                  │                  │
        │             Ranked/Unranked     Tournament
        │                  │              Scheduling
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │
                     Match/session
                           │
                 P2P signaling / setup
                           │
              ┌────────────┴────────────┐
              │                         │
           Player A                  Player B
              │                         │
              └──── existing GGPO ─────┘
                           │
                   deterministic replay
                           │
                     600-frame delay
                           │
                           ▼
                        Nakama
                           │
                 delayed input stream
                           │
                   ┌───────┼───────┐
                   ▼       ▼       ▼
               spectator spectator spectator
                   │       │       │
                   └── IKEMEN deterministic playback ──┘
```

Nakama is intended to be the control plane and social/matchmaking service. It is not intended to become the live fighting-game simulation server.

## 2. Implemented Features

### 2.1 Nakama client integration

Added an engine-side Nakama client in `src/nakama_client.go`.

Implemented capabilities include:

- REST authentication using device/session credentials.
- Realtime WebSocket connection.
- WebSocket read/write loops.
- Event dispatch from Nakama into IKEMEN.
- Match join/leave.
- Match data send operations.
- Server RPC calls.
- Lobby listing through Nakama authoritative match listing.
- Chat channel join/send.
- P2P signaling messages.
- Replay header/chunk/end/reset messages.

The client is exposed to Lua through `src/nakama_script.go`.

### 2.2 Ranked matchmaking

The Nakama server module `nakama/modules/ikemen_matchmaker.lua` implements ranked and unranked matchmaking.

Ranked matchmaking:

- Groups tickets by game, build, mode, and region.
- Retrieves the player's rating from server-side storage.
- Compares rating distance against the configured allowed range.
- Chooses a compatible opponent.
- Creates a Nakama `ikemen_session` match for the two players.

The client can provide an Elo range as matchmaking metadata, but the authoritative rating is intended to come from server-side storage.

### 2.3 Unranked matchmaking

Unranked matchmaking uses the same matchmaker infrastructure while ignoring Elo distance when pairing compatible tickets.

The matchmaking target is two players.

### 2.4 Server-side Elo

Added `nakama/modules/ikemen_rating.lua`.

The module provides:

- Per-game rating storage.
- Configurable initial rating.
- Configurable K-factor.
- Configurable rating scale.
- Wins/losses/game counters.
- Reciprocal result submission.
- Match-specific settlement protection so the same match is not intended to change ratings twice.
- Optional replay hash agreement checking.

Default configuration currently is:

```lua
initial_rating = 1000
k_factor = 32
rating_scale = 400
```

Game-specific values can be placed in the `GAME_CONFIG` table.

This is not a cryptographic anti-cheat system. It verifies reciprocal result claims, but two cooperating clients could still submit a false matching result unless a separate authoritative replay verification system is added.

### 2.5 Authoritative 8-player lobbies

Added `nakama/modules/ikemen_lobby.lua`.

A lobby supports up to eight players and keeps authoritative state for:

- Player membership/order.
- Lobby name.
- Game/build identity.
- Lobby format.
- Rules metadata.
- Current pairing.
- Tournament state.
- Result progression.

Supported scheduling formats are:

```text
winner_stays_on
round_robin
bracket
```

The lobby coordinates pairings but does not simulate the fight.

### 2.6 Lobby browser

The Lua API exposes `nakama.listLobbies()` through Nakama's match listing API.

The lobby browser is intentionally not implemented as a separate hardcoded renderer. Screenpacks/game scripts can use the returned lobby data to construct their own interface using normal IKEMEN Lua/UI facilities.

### 2.7 Nakama chat

Chat access is provided through the Nakama client:

```lua
nakama.joinChat(target)
nakama.sendChat(channel_id, message)
```

Incoming chat events are exposed to Lua through the Nakama event callback system.

### 2.8 P2P / NAT traversal primitive

Added `src/nakama_p2p.go`.

The current implementation provides:

- Local IPv4 candidate discovery.
- STUN Binding Request support.
- Server-reflexive address discovery.
- UDP hole-punching.
- Shared handshake token validation.
- Persistent UDP socket retention after the P2P connection succeeds.

The P2P object exposes the established UDP connection and remote endpoint so it can eventually be attached to the GGPO transport without closing/rebinding the NAT mapping.

Lua access is currently exposed as:

```lua
nakama.startP2P(port, stun_servers)
nakama.stopP2P()
```

The player is not expected to manually configure the connection method in the intended final flow.

## 3. Existing IKEMEN/GGPO Systems Retained

The implementation deliberately leaves the following existing mechanisms in place:

- IKEMEN deterministic game simulation.
- GGPO rollback simulation.
- Existing GGPO session/state logic.
- Existing UDP gameplay transport.
- Existing TCP-derived synchronization path.
- Existing replay file system.
- Existing Lua runtime.
- Existing screenpack/motif architecture.

The goal is to use Nakama to automate coordination rather than replace these systems unnecessarily.

## 4. Replay and Spectator System

### 4.1 Ten-second delayed spectator model

The default spectator publication delay is configured in `src/resources/defaultConfig.ini`:

```ini
Rollback.ReplayBroadcastDelay = 10
```

The replay stream operates at 60 frames per second, so the default delay is:

```text
60 FPS × 10 seconds = 600 frames
```

This delay is substantially larger than IKEMEN GO's rollback correction window and provides a buffer before replay input is exposed to spectators.

### 4.2 Existing replay representation reused

The spectator stream reuses the existing deterministic IKEMEN replay input representation instead of introducing a video stream or a separate game-state replication protocol.

The stream is represented by:

- replay header
- input frame chunks
- replay end message
- replay reset message

Replay chunks are currently 30 frames each.

The existing input representation is reused at eight controller slots and eight bytes of input data per controller per frame.

### 4.3 Rollback-aware publication

`src/replay_stream.go` implements `RollbackReplayStream`.

The rollback session invokes it from the existing replay recording path in `src/rollback.go`.

The stream:

1. Records inputs from the existing rollback timeline.
2. Waits for the configured publication delay.
3. Publishes only input frames that are old enough to be considered settled.
4. Tracks replay truncation.
5. Sends a reset when a rollback reaches data that has already been considered published.

This is intended to prevent speculative rollback data from becoming permanent spectator data.

### 4.4 Spectator-side buffering

`NakamaReplayBuffer` receives and retains replay stream data on the client.

It tracks:

- replay header
- contiguous buffered frame range
- final frame
- reset reason
- individual frame inputs

It can wait for a particular frame to arrive before replay playback consumes it.

### 4.5 Native replay playback

`src/netplay.go` extends `ReplayFile` with a live-buffer mode.

`NewLiveReplayFile()` wraps a `NakamaReplayBuffer` as a `ReplayFile` source.

`ReplayFile.Synchronize()` and `ReplayFile.Update()` recognize live replay buffers and feed the buffered deterministic inputs through the same replay playback path used for ordinary replay files.

The intent is therefore:

```text
Nakama replay chunks
        ↓
NakamaReplayBuffer
        ↓
NewLiveReplayFile
        ↓
existing ReplayFile playback
        ↓
IKEMEN deterministic simulation
```

No separate spectator renderer or video decoder is used.

## 5. Spectator Loading Screen

The ten-second spectator delay is treated as a loading/buffering period instead of showing the underlying game immediately.

The engine tracks the loading state in `System` and waits for the delayed replay buffer to reach the configured delay before beginning replay playback.

The engine-side loading state is exposed through:

```lua
motifState("nakama_spectator_loading")
```

The current implementation also exposes replay status through:

```lua
nakama.replayStatus()
```

including values such as:

- `buffered_through`
- `delay_frames`
- `ready`
- `loading`
- `frame_rate`
- `final_frame`
- `ended`
- `reset_reason`

## 6. Screenpack Integration

Nakama UI that is rendered by the engine is integrated into the motif/screenpack system rather than using fixed engine-only presentation values.

Added to `src/motif.go`:

```text
NakamaInfoProperties
NakamaInfo
NakamaBgDef
```

Added to `src/resources/defaultMotif.ini`:

```ini
[Nakama Info]
[NakamaBgDef]
```

### 6.1 Configurable Nakama loading presentation

The `[Nakama Info]` section provides screenpack-controlled properties for:

- title text
- loading text
- progress text
- remaining-time/detail text
- fonts
- offsets
- scales
- facing
- layers
- windows
- localcoord
- projection parameters
- fade properties
- status text
- error text

Example:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING
```

### 6.2 Configurable background

`[NakamaBgDef]` can provide a dedicated background definition for Nakama screens.

Existing screenpacks remain compatible because the spectator loading text can fall back to the existing `[Title Info]` loading/wait motif when the Nakama-specific element is disabled or unavailable.

### 6.3 Lua-driven screens

Lobby browser, lobby player list, tournament bracket, chat, online status, matchmaking, and similar screens are not hardcoded as a second menu system.

They are intended to be built by the screenpack/game's Lua code from Nakama data and events.

This keeps screenpack design responsibility in the same place as other IKEMEN menus rather than forcing one engine-defined visual style.

## 7. Lua API Added

`src/nakama_script.go` registers a global `nakama` table during `systemScriptInit()`.

Current functions include:

```lua
nakama.connect(config)
nakama.disconnect()
nakama.on(event, callback)

nakama.matchmake(mode, elo, elo_range, game, build, region)
nakama.cancelMatchmaking()

nakama.joinMatch(match_id, token)
nakama.leaveMatch()

nakama.startP2P(port, stun_servers)
nakama.stopP2P()

nakama.replayStatus()
nakama.startSpectatorReplay()

nakama.currentMatch()
nakama.userId()
nakama.username()

nakama.sendMatchData(opcode, data, reliable)
nakama.sendMatchDataBase64(opcode, base64, reliable)

nakama.joinChat(target, channel_type, persistence, hidden)
nakama.sendChat(channel_id, message)

nakama.listLobbies(query, label, min_size, max_size, limit)
nakama.createLobby(config)
nakama.reportLobbyResult(winner_id, loser_id)

nakama.submitResult(match_id, opponent_id, game, result, replay_hash)
```

## 8. Lua Event Model

The Nakama client parses realtime messages off-thread.

Lua callbacks are not invoked directly from the Nakama WebSocket goroutine.

Instead the implementation queues callback work onto IKEMEN's main-thread task mechanism.

The general flow is:

```text
Nakama WebSocket goroutine
        ↓
parse event
        ↓
IKEMEN mainThreadTask queue
        ↓
Lua callback
```

This avoids directly touching the gopher-lua state from a background network goroutine.

The P2P signaling path is also handled separately enough that the P2P handshake does not depend on Lua callbacks running while the game thread is waiting for synchronization.

## 9. Server Modules

### `nakama/modules/ikemen_matchmaker.lua`

Handles:

- ranked/unranked ticket processing
- game/build/region buckets
- server-side Elo lookup
- ranked opponent selection
- creation of `ikemen_session` matches

### `nakama/modules/ikemen_session.lua`

Acts as the Nakama coordination match for a matched 1v1 session.

Handles:

- expected players
- membership
- match data relay
- P2P signaling messages
- replay stream messages

It does not simulate IKEMEN gameplay.

### `nakama/modules/ikemen_lobby.lua`

Handles:

- lobby membership
- maximum eight players
- player ordering
- winner-stays-on scheduling
- round-robin scheduling
- bracket scheduling
- current pairing
- result progression
- lobby state broadcasts

### `nakama/modules/ikemen_rating.lua`

Handles:

- server-side rating storage
- Elo calculation
- reciprocal result validation
- duplicate settlement protection
- optional replay hash agreement

## 10. Code Changes by File

### Added files

| File | Purpose |
|---|---|
| `src/nakama_client.go` | Nakama REST/realtime client, matchmaking, lobby access, chat, signaling, replay transport hooks. |
| `src/nakama_p2p.go` | STUN, candidate gathering, UDP hole punching, P2P connection lifecycle. |
| `src/nakama_script.go` | Lua bindings and Lua event dispatch for Nakama functionality. |
| `src/replay_stream.go` | Rollback-aware delayed replay stream and client-side replay buffer. |
| `nakama/modules/ikemen_matchmaker.lua` | Ranked/unranked Nakama matchmaking processor. |
| `nakama/modules/ikemen_session.lua` | Coordination match for matched players and stream/signaling messages. |
| `nakama/modules/ikemen_lobby.lua` | Authoritative eight-player lobby and tournament scheduler. |
| `nakama/modules/ikemen_rating.lua` | Server-side Elo storage and result settlement. |
| `nakama/examples/ikemen_nakama.lua` | Example Lua integration/event registration. |
| `nakama/README.md` | Integration notes and usage documentation. |

### Modified files

| File | Changes |
|---|---|
| `src/rollback.go` | Connects the rollback session to the delayed replay stream, records frames when replay streaming is enabled, propagates replay truncation, ends the stream at match completion, and initializes the stream from configuration. |
| `src/netplay.go` | Stores synchronized replay metadata and exposes `ReplayFile` live-buffer mode for deterministic spectator playback. |
| `src/system.go` | Adds Nakama client state, spectator loading state, loading-screen rendering, delayed replay startup, and Nakama state rendering. |
| `src/motif.go` | Adds Nakama motif properties and optional Nakama background definition. |
| `src/resources/defaultMotif.ini` | Adds default Nakama screenpack configuration. |
| `src/resources/defaultConfig.ini` | Adds `Rollback.ReplayBroadcastDelay = 10`. |
| `src/script.go` | Registers the Nakama Lua API and exposes Nakama-related motif states. |
| `go.mod` | Pass 5 changed the Go language version from `1.27.0` to `1.23.2` as a local environment workaround. This is not the intended project requirement and should be restored to the project's required Go version for normal builds. |

## 11. Existing Network Transport and Current Integration Boundary

The implementation intentionally did not replace the external GGPO package's transport in the supplied tree.

The current IKEMEN rollback setup still uses the existing GGPO construction path and its existing UDP transport.

The new `NakamaP2P` object can establish and retain a usable UDP socket and exposes:

```text
UDPConn()
RemoteAddr()
```

The intended final direct-connection integration is:

```text
Nakama match
    ↓
P2P signaling
    ↓
STUN / UDP hole punch
    ↓
retained UDP socket
    ↓
existing GGPO transport boundary
    ↓
existing GGPO rollback session
```

The current source has not yet completed the last step of passing the already-established socket directly into the external GGPO transport constructor.

For this reason, the P2P code currently represents a connection-establishment primitive rather than a finished replacement for IKEMEN's existing gameplay transport.

## 12. Result Reporting and Ranked Integrity

The client can submit match results and a replay hash.

The Nakama rating module waits for reciprocal claims and requires:

- matching match identity
- matching opponent identity
- matching game identity
- complementary results
- matching replay hashes when both clients provide hashes

When those conditions are met, Elo is updated for both players.

This prevents simple one-sided accidental result updates and duplicate settlement, but it does not independently prove that the reported replay or result is truthful.

A future authoritative deterministic verifier could run the final replay against a compatible headless IKEMEN build before accepting ranked results. That verifier is not implemented in the current tree.

## 13. Compatibility / Sync Considerations

The existing IKEMEN synchronized replay header carries sync settings and a content fingerprint field.

However, the current engine implementation still contains a `currentContentFingerprint()` TODO and currently returns an empty string.

For production ranked matching, the fingerprint should be made meaningful and should cover all simulation-relevant content/build inputs before treating it as a reliable compatibility gate.

The matchmaking identity already carries game/build information so incompatible client builds can be separated at the Nakama layer.

## 14. Current Limitations

The current Pass 5 tree is an integration prototype rather than a finished production service.

The remaining major work is:

1. **Attach the retained NAT-traversed UDP socket to the external GGPO transport.**
   The current code stops at the transport handoff boundary.

2. **Automatic fallback behavior for networks where direct traversal fails.**
   A TURN/relay fallback has not been implemented.

3. **Fully generic live spectator initialization.**
   The deterministic input buffer and live `ReplayFile` support are present, but arbitrary game-specific replay initialization still depends on the game's setup/Lua path.

4. **New spectator joins.**
   The current documentation/implementation assumes a lobby member can receive the stream from its beginning. Historical replay backfill for a user who joins after the match has already started is not yet a complete protocol.

5. **Authoritative ranked replay verification.**
   Reciprocal claims and replay hashes are not a replacement for deterministic server verification.

6. **Production Nakama deployment configuration.**
   The Lua modules are supplied as integration modules; production deployment still requires configuration of the actual Nakama server, authentication, storage, runtime module loading, and deployment-specific STUN/relay infrastructure.

## 15. Validation Status

The supplied Pass 5 source tree was compared against the original IKEMEN GO source tree supplied for the project.

Pass 5 adds approximately 3,700 lines of new code/configuration and modifies the existing engine in a relatively small number of locations. Most of the new functionality lives in new files or Nakama modules rather than invasive changes to core combat/simulation code.

Static source parsing/gofmt checks were performed on the modified Go sources during development.

A complete engine build was not established against the original project's declared Go requirement in this environment. Pass 5 temporarily changed the `go` directive in `go.mod` to `1.23.2` to match the available local toolchain; this should not be treated as the correct project toolchain requirement. A Go 1.27.1 source archive was subsequently supplied for the environment, but the Pass 5 tree was not rebuilt and repackaged after switching toolchains.

## 16. Design Principle

The implementation is intentionally structured around the smallest useful bridge between Nakama and IKEMEN GO:

```text
Nakama:
    identity
    matchmaking
    ratings
    lobbies
    tournaments
    chat
    signaling
    delayed replay distribution

IKEMEN:
    simulation
    GGPO rollback
    UDP gameplay
    replay playback
    Lua
    screenpacks
```

The objective is to add online-service functionality without creating a second fighting-game networking stack, a second replay system, or a second screen/menu framework when the existing IKEMEN systems can perform the required job.


## Pass 6 continuation — recovered work and current delta

Pass 6 continues directly from the supplied Pass 5 tree. It does not replace the existing IKEMEN/GGPO simulation architecture.

### Recovered Pass 5 work preserved

The supplied Pass 5 tree already contains the Nakama client, ranked/unranked matchmaking, server-side Elo module, authoritative lobbies, chat, P2P/STUN/hole-punching, delayed replay streaming, spectator loading presentation, Lua bindings, replay synchronization, and the synchronization/rollback changes described above. Those systems remain the baseline.

### Pass 6 changes

1. `NakamaReplayBuffer.ApplyChunk` now advances the contiguous-frame boundary after every chunk. Previously the advancement path was limited to an initial-frame case, which could leave `BufferedThrough` stalled after the first chunk.

2. `NakamaReplayBuffer.Reset` now clears the received replay header as well as frames and end-state data. A reset therefore requires a new replay header before spectator startup can proceed.

3. The Nakama client can transfer ownership of an established `NakamaP2P` UDP socket through `TakeP2PTransport`. The transfer removes the socket from the P2P object's ownership before it is closed.

4. Rollback startup now prefers the retained Nakama UDP socket when the client is a member of a Nakama match and P2P is ready. It verifies that the P2P socket uses the configured `Rollback.Port`, obtains the actual observed remote UDP endpoint, and passes the retained socket to the GGPO transport.

5. `RollbackSession.InitP1` and `InitP2` accept an optional pre-bound UDP socket. Legacy GGPO socket creation remains the fallback when no pre-bound socket is supplied.

6. The spectator loading renderer no longer resets animated text every frame. Text sprites are updated once per frame, and Nakama-specific loading text falls back to the established title loading/wait text when the Nakama text is empty.

7. Strict synchronization validation now rejects a strict-setting schema length/path mismatch before value comparison. This closes the missing-field compatibility hole in the earlier strict comparison.

8. Realtime Lua callbacks are explicitly marshalled onto the engine main-thread task queue instead of invoking gopher-lua from a network goroutine. The queue is best-effort and a saturated queue drops the event rather than blocking the networking goroutine. Callback storage is local to the Lua setup instead of being held in a package-global map.

9. `JoinMatchWithMetadata` supplies the configured game/build identity when the caller does not provide them and no longer mutates the caller's metadata map.

10. Nakama Lua server modules now explicitly import `require("nakama")`, and `nakama/modules/main.lua` registers the authoritative `ikemen_session` and `ikemen_lobby` handlers while loading the matchmaker and rating modules.

11. Lobby join validation now requires game/build identity to match the lobby whenever those lobby identities are populated.

12. The lobby bracket scheduler now consumes automatic byes and advances to the next real match instead of leaving a `player2=nil` pairing active.

13. Nakama `dispatcher.broadcast_message` calls were normalized to the documented four-argument form.

14. `RollbackReplayStream.End` now flushes all remaining authoritative replay frames before publishing the final-frame marker. This closes a short-match/early-end case where the normal ten-second publication window had not elapsed before the fight ended.

15. `currentContentFingerprint()` now produces a deterministic SHA-256 digest from the engine version, active motif/fight-screen definitions, loaded character definitions and character FightFX, the active stage and attached-character definitions, and loaded common FightFX. The digest uses role labels and file contents rather than absolute installation paths.

### External GGPO boundary

The Pass 6 source now contains the IKEMEN-side transport handoff. The remaining dependency-side change is intentionally small: the pinned `github.com/ikemen-engine/ggpo` transport package must expose a constructor that wraps an already-open `net.PacketConn` without binding a second UDP socket. The implementation requirement is documented in `patches/ggpo-nakama-transport.md`.

The dependency is not vendored in the supplied repository and could not be fetched in this environment, so the constructor itself has not been compiled against the exact pinned GGPO source here.

### Ranked-result integrity correction

The earlier Pass 5 description of duplicate settlement protection was stronger than the implementation supports. The current Elo module stores a settlement marker per submitting user. Two reciprocal RPCs can race before either user's marker exists, so the marker is not an atomic match-wide claim. The implementation should therefore be treated as reciprocal-claim reconciliation, not complete duplicate-settlement protection, until settlement is moved into a single authoritative match path or a verified atomic compare-and-swap/transactional storage mechanism is used.

### Validation state

The project `go.mod` is restored to `go 1.27.0`. The available local compiler is Go 1.23.2, and the exact pinned GGPO source is not present in the local module cache. A full engine test was attempted with `GOTOOLCHAIN=local go test ./src` and is blocked immediately because the local toolchain is below the module's declared minimum. Consequently a full engine build has not been claimed.

Focused validation completed successfully:

- standalone P2P ownership tests: `go test ./...` → pass
- standalone replay-buffer/replay-stream tests, including final-frame flushing: `go test ./...` → pass
- Lua syntax checks for all Nakama modules and the example: all pass

### Recovered continuation state

This pass was reconstructed from the supplied Pass 5 source tree plus the later implementation material available in the project. The source tree, rather than an unavailable prior-chat transcript, is the authoritative record for code that is claimed as implemented here.

The next dependency-side blocker remains the pinned GGPO transport adapter. The content fingerprint is now meaningful and hashes the engine version plus active match definitions and loaded FightFX, but it is intentionally not claimed as an exhaustive digest of every dynamically loaded asset or runtime Lua file.


## Appendix B — Pass 7 historical record


## 1. Current architecture

Nakama supplies identity, matchmaking, ratings, lobbies, tournaments, chat,
signaling, and delayed replay distribution. IKEMEN GO remains responsible for
deterministic simulation, GGPO rollback, gameplay transport, replay playback,
Lua scripting, and screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket
is transferred into GGPO without rebinding so that the NAT traversal state is
preserved.

## 2. Recovered Pass 5/6 implementation

The current tree retains the previously completed work for:

- synchronized configuration schemas and session overrides;
- rollback UDP-port negotiation and compatibility checks;
- replay synchronization headers and restoration;
- content/build fingerprinting;
- Nakama client, matchmaking, lobby, chat and result APIs;
- Nakama Lua server modules and `modules/main.lua` registration;
- Lua-thread-safe event dispatch;
- STUN candidate gathering and UDP hole punching;
- delayed spectator replay buffering and loading presentation;
- replay final-frame flushing;
- Lua bindings for Nakama functionality.

## 3. Pass 7 changes

### 3.1 Bundled GGPO transport adapter

The exact pinned GGPO source is bundled under `third_party/ggpo` and the root
`go.mod` replaces the remote module with that local copy for reproducible builds.

Added:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

The constructor uses the existing GGPO `Udp` implementation and sender/receiver
lifecycle while adopting an already-bound `net.PacketConn`. It does not create a
second socket. The existing `NewUdp` constructor delegates to the same internal
initializer after binding its own socket.

### 3.2 Nakama P2P self-signal protection

Nakama match broadcasts can be delivered to the publishing client. The P2P
signal handler now ignores a signal whose token is the local token instead of
adopting the local socket as the remote peer.

### 3.3 Focused P2P regression coverage

Added engine-side tests for:

- ignoring self-originated P2P signals;
- completing a two-peer localhost UDP handshake;
- recording the actual observed remote endpoint for each peer.

## 4. GGPO verification

The bundled GGPO transport package passes its lifecycle tests, including the new
`NewUdpFromPacketConn` test covering socket reuse, unchanged local port, packet
receipt, close/unblock, and subsequent port rebind.

The GGPO core packages compile in an isolated validation copy. Existing upstream
GGPO tests unrelated to the transport adapter have pre-existing behavioral
failures in that validation environment; those failures are not represented as
transport-adapter failures.

## 5. Remaining validation boundary

A complete IKEMEN engine build still requires an actual Go 1.27.x compiler and the
project's native/build dependencies. The supplied Go 1.27.1 source archive is not
itself a compiled bootstrap toolchain, and the environment lacks the required
Go 1.24.6-or-newer bootstrap compiler.

The next real acceptance test is an end-to-end two-client match using a real
Nakama server and the bundled GGPO transport:

```text
Nakama authentication
    -> matchmaker
    -> ikemen_session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO transport
    -> rollback fight
    -> replay publication
```

## 6. Known limitations

TURN/relay fallback is not implemented. Late-joining spectator backfill is not
a complete protocol. Ranked-result handling still uses reciprocal claims and is
not an authoritative deterministic replay verifier. The content fingerprint
covers the configured active match/build definitions and loaded FightFX assets,
but it is not claimed to be an exhaustive digest of every dynamically loaded
runtime asset or Lua module.

The Elo settlement path remains non-atomic: two reciprocal submissions can race
before a match-wide settlement marker exists. It should therefore not be treated
as complete duplicate-settlement protection for a public ranked service.

## 7. Build integration

The development tree contains the GGPO source at `third_party/ggpo` and uses:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The standalone dependency patch is also supplied at:

```text
patches/ggpo-nakama-transport.patch
```

so the same change can be applied to the upstream GGPO module instead of using
the bundled source when the dependency is maintained externally.


---

# Pass 9

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass9.md`

# Current Pass 9 Status

This document preserves the earlier Pass 6–8 implementation history below and then records the Pass 9 continuation at the end. Statements in the historical sections describing missing GGPO transport support refer to the state at that earlier pass; the current tree now bundles the modified GGPO dependency under `third_party/ggpo`.

Current Pass 9 additions are the Nakama-coordinated online rematch flow, three-way post-match choice UI, VS snapshot reuse, fresh P2P/GGPO session sequencing, ranked side-switch snapshot transformation, ranked final-match suppression, and server-side snapshot validation/consistency checks.

# IKEMEN GO + Nakama — Consolidated Implementation Record, Pass 8

## 1. Record scope

This file is the consolidated implementation record for the current development tree. It supersedes the separate Pass 6 and Pass 7 summaries for status tracking while preserving their detailed historical material in the appendices. The source tree is authoritative for what is actually present in code.

## 2. Current architecture

Nakama provides account identity, matchmaking, lobby/presence, chat, signaling, ratings/result plumbing, and delayed replay distribution. IKEMEN GO remains responsible for deterministic simulation, GGPO rollback, gameplay transport, replay playback, Lua execution, renderer composition, and game/screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket is transferred into GGPO without rebinding so that the NAT traversal state is preserved.

The optional border/presentation system is render-only. It provides a physical outer canvas, independently configured game viewport, border-authored assets, and Lua render hooks. It does not participate in rollback state or deterministic simulation.

Ranked sets are session/presentation state. Best-of-X, side switching, and winner-selection locking are negotiated outside rollback and propagated through Nakama matchmaking/session metadata.

## 3. Completed implementation through Pass 8

### 3.1 Synchronization and session overrides

The tree contains synchronized configuration schemas, strict/host scopes, deterministic serialization/validation, session-wide overrides, restoration, synchronization warnings, effective fight-aspect compatibility checks, replay synchronization headers, rollback UDP-port negotiation, and content/build fingerprint plumbing.

### 3.2 Nakama integration

The tree contains the Nakama client, matchmaking, lobby/presence, chat, result plumbing, server Lua modules, `modules/main.lua` registration, Lua-thread-safe event dispatch, STUN candidate gathering, UDP hole punching, P2P signaling, delayed spectator replay loading, and Lua bindings for Nakama/session information.

Nakama match broadcasting is protected against accepting the local client's own P2P signal.

### 3.3 GGPO transport integration

The pinned GGPO source is bundled in `third_party/ggpo` and the root module uses a local replacement:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The GGPO transport exposes:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

This adopts the already-bound Nakama/STUN `PacketConn` without creating a second UDP socket. The existing GGPO constructor delegates to the same initializer after binding its own socket.

The transport lifecycle test verifies socket reuse, unchanged local port, packet receipt, close/unblock, and subsequent port rebind.

### 3.4 Presentation canvas / border system

The engine now distinguishes the physical presentation canvas from the game viewport.

When border presentation is enabled:

```text
outer canvas = Video.Border.Width  × Video.Border.Height
game viewport = Video.GameWidth     × Video.GameHeight
```

The game viewport is centered inside the border by default. Signed `Video.Border.GameOffsetX` and `Video.Border.GameOffsetY` move it relative to that centered position.

Command-line controls:

```text
-bordered
-borderwidth <pixels>
-borderheight <pixels>
-bordergamex <pixels>
-bordergamey <pixels>
```

Normal non-bordered resolutions continue to use the game resolution as the output resolution. Bordered resolution presets bind an outer resolution to an independent game resolution. Current defaults include:

```text
1280x720  -> 960x720
1600x900  -> 1200x900
1920x1080 -> 1440x1080
1920x1200 -> 1600x1200
2560x1440 -> 1920x1440
```

The options system has separate `Resolution` and `Bordered Resolutions` menus. Bordered presets enable presentation and reset placement offsets. Custom bordered resolution accepts outer width/height and game width/height independently.

The renderer paths were updated for OpenGL 3.3, OpenGL ES 3.2, and Vulkan. Font rendering uses the active presentation viewport. Scissor rectangles account for the presentation origin.

### 3.5 Border authoring

The motif now contains a `[Border]` section with separate background and foreground layers. Each layer uses the engine's existing animation, text, and rectangle primitives.

The intended syntax is:

```text
[Border]
enabled = 1
localcoord = 1920, 1080
background.anim = ...
background.text.text = ...
background.overlay.col = ...
background.overlay.alpha = ...
foreground.anim = ...
foreground.text.text = ...
foreground.overlay.col = ...
foreground.overlay.alpha = ...
```

`BorderLayerProperties` uses the existing `AnimationProperties` directly plus grouped `text` and `overlay` sections. The border's own `localcoord` is the inherited coordinate space for child animation/text/overlay data unless a child explicitly supplies its own localcoord.

The border layer updates animation, text, and rectangle state each render frame and renders across the existing layer numbers. Render hooks are:

```text
game.border_bg
game.border
```

They execute on the outer presentation canvas. Existing Lua animation, text, and rectangle APIs can therefore be used to construct dynamic side panels, player information, chat, spectator data, challenge displays, and other UI without coupling those elements to the rollback simulation.

Lua accessors include:

```text
borderEnabled()
borderWidth()
borderHeight()
borderGameX()
borderGameY()
borderGameWidth()
borderGameHeight()
```

The border is deliberately not implemented as a conventional OS window border. It is a composition surface around the gameplay viewport so game and border resolutions remain independently addressable.

### 3.6 Ranked best-of-X sets

`Netplay.Ranked` contains:

```text
BestOf
SwitchSides
WinnerKeepsSelection
```

`BestOf` is normalized to an odd value from 1 through 99.

The matchmaker includes the negotiated ranked rules in its compatibility bucket and session label so players with different set rules are not silently paired into one rule set.

A render/session-only `RankedSet` object tracks:

- stable player identities;
- current physical side assignment;
- wins and completed match count;
- set winner;
- immediate previous match winner;
- character/team selection tokens.

A set completes as soon as one player reaches the majority required by BestOf. When `SwitchSides` is enabled, physical side assignment swaps only after a non-terminal match. Local controller mappings are then swapped so player identity follows the assigned side.

When `WinnerKeepsSelection` is enabled, the previous match winner's confirmed character/team selection is carried into the next match after the side assignment is resolved. The loser remains selectable.

The selection carry is applied before the next team selection menu is constructed, and the finished set exits the repeated-match loop when a majority winner is reached.

### 3.7 Ranked Lua/session API

The Nakama Lua surface exposes ranked-set state and actions through:

```text
nakama.rankedSet()
nakama.rankedSetRecordMatch(winnerSide)
nakama.rankedSetSelectionLocked(player, token)
```

The state table includes set activity, BestOf, match count, win counts, player IDs, physical side mapping, switch-side policy, winner-selection policy, set winner, and previous match winner.

### 3.8 Replay and spectator behavior

Delayed replay distribution remains integrated with rollback. The replay buffer flushes final delayed frames before the live stream is terminated, including matches shorter than the normal spectator delay.

Spectator replay startup, loading UI, replay synchronization headers, and session configuration restoration remain part of the existing implementation.

### 3.9 Content fingerprint

The content fingerprint now produces a deterministic SHA-256 over the configured active match/build definitions and loaded FightFX assets. Machine-specific absolute paths are not used as the hash identity.

The implementation is intentionally not described as an exhaustive digest of every dynamically loaded runtime asset or Lua module; that remains a separate hardening task.

## 4. Corrective fixes made during Pass 8 continuation

### 4.1 Ranked-set reset state

`RankedSet.Reset()` now explicitly restores both `winner` and `lastWinner` to `-1`. This prevents a reset ranked session from exposing a stale previous-winner state through Lua.

A regression test covers the reset behavior.

### 4.2 Border motif field layout

The border animation definition is flattened to use the same convention as other motif animation properties (`background.anim`, etc.) while keeping text and overlay grouped under their respective keys.

### 4.3 Border localcoord inheritance

The data-pointer population path recognizes `BorderInfoProperties.Localcoord` as the coordinate-space root for border children. Explicit per-element localcoord values remain authoritative.

### 4.4 Border rectangle animation

Border overlay rectangles are updated every render frame so pulsing alpha and related rectangle state animate correctly.

### 4.5 Lua hook syntax correction

The presentation-hook documentation comment was corrected so it does not become part of a malformed Lua function declaration.

## 5. Validation performed

### Passed

- ranked-set standalone tests using the available Go 1.23 compiler:
  - BestOf normalization;
  - majority set completion;
  - side switching;
  - winner-selection lock;
  - reset clears previous winner.
- P2P self-signal rejection test.
- Two-peer localhost UDP punching/handshake test.
- GGPO `NewUdpFromPacketConn` lifecycle test.
- GGPO core package compilation in an isolated validation copy.
- Replay-buffer final-frame flush regression test.
- Source formatting with `gofmt` for modified Go files.

### Not yet fully validated

A complete IKEMEN engine build/test run has not been completed in this environment because the project requires Go 1.27.0+ and the local compiler is Go 1.23.2. The supplied Go 1.27.1 archive is source code and itself requires a Go 1.24.6-or-newer bootstrap compiler.

The environment also has no network access for automatic module/toolchain acquisition and no installed Lua interpreter for an independent Lua parser/runtime check.

## 6. Build status

The source tree is structured for the project's existing platform build scripts. The bundled GGPO dependency is part of the source tree and does not require a separately maintained fork at build time.

Builds still require the appropriate target toolchain and native dependencies described by `BUILDING.md` for Windows, Linux, macOS, and Android.

The current tree should therefore be treated as a development/test release candidate rather than as a fully cross-compiled and end-to-end validated public release.

## 7. Remaining engineering work

The major remaining online acceptance boundary is a real two-client session:

```text
Nakama authentication
    -> matchmaking
    -> session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO
    -> real rollback fight
    -> replay publication
```

Known limitations remain:

- TURN/relay fallback is not implemented;
- late-joining spectator backfill is incomplete;
- ranked result handling is not yet an authoritative deterministic replay verifier;
- Elo/result settlement is not atomic duplicate-settlement protection;
- content fingerprinting is not exhaustive of every dynamic runtime resource.

## 8. Pass 8 design rules

The border feature must remain presentation-only. Dynamic border information may include player data, online status, chat, spectators, lobby state, challenge state, and other non-deterministic UI information, but it must not directly mutate rollback gameplay state during a synchronized fight.

Border presentation should remain disabled without opt-in and must preserve ordinary non-bordered behavior when disabled.

Ranked set rules are session state, not rollback state. The game simulation remains governed by the existing deterministic synchronization and GGPO mechanisms.

## Appendix A — Pass 6 historical record


## 1. Overview

This project adds a Nakama-backed online service layer to IKEMEN GO while retaining IKEMEN GO's existing deterministic simulation, GGPO rollback system, UDP gameplay transport, replay system, Lua scripting model, and screenpack/motif system as the primary game-side machinery.

The intended architecture is:

```text
                         NAKAMA
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   Authentication      Matchmaking         Lobbies
        │                  │                  │
        │             Ranked/Unranked     Tournament
        │                  │              Scheduling
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │
                     Match/session
                           │
                 P2P signaling / setup
                           │
              ┌────────────┴────────────┐
              │                         │
           Player A                  Player B
              │                         │
              └──── existing GGPO ─────┘
                           │
                   deterministic replay
                           │
                     600-frame delay
                           │
                           ▼
                        Nakama
                           │
                 delayed input stream
                           │
                   ┌───────┼───────┐
                   ▼       ▼       ▼
               spectator spectator spectator
                   │       │       │
                   └── IKEMEN deterministic playback ──┘
```

Nakama is intended to be the control plane and social/matchmaking service. It is not intended to become the live fighting-game simulation server.

## 2. Implemented Features

### 2.1 Nakama client integration

Added an engine-side Nakama client in `src/nakama_client.go`.

Implemented capabilities include:

- REST authentication using device/session credentials.
- Realtime WebSocket connection.
- WebSocket read/write loops.
- Event dispatch from Nakama into IKEMEN.
- Match join/leave.
- Match data send operations.
- Server RPC calls.
- Lobby listing through Nakama authoritative match listing.
- Chat channel join/send.
- P2P signaling messages.
- Replay header/chunk/end/reset messages.

The client is exposed to Lua through `src/nakama_script.go`.

### 2.2 Ranked matchmaking

The Nakama server module `nakama/modules/ikemen_matchmaker.lua` implements ranked and unranked matchmaking.

Ranked matchmaking:

- Groups tickets by game, build, mode, and region.
- Retrieves the player's rating from server-side storage.
- Compares rating distance against the configured allowed range.
- Chooses a compatible opponent.
- Creates a Nakama `ikemen_session` match for the two players.

The client can provide an Elo range as matchmaking metadata, but the authoritative rating is intended to come from server-side storage.

### 2.3 Unranked matchmaking

Unranked matchmaking uses the same matchmaker infrastructure while ignoring Elo distance when pairing compatible tickets.

The matchmaking target is two players.

### 2.4 Server-side Elo

Added `nakama/modules/ikemen_rating.lua`.

The module provides:

- Per-game rating storage.
- Configurable initial rating.
- Configurable K-factor.
- Configurable rating scale.
- Wins/losses/game counters.
- Reciprocal result submission.
- Match-specific settlement protection so the same match is not intended to change ratings twice.
- Optional replay hash agreement checking.

Default configuration currently is:

```lua
initial_rating = 1000
k_factor = 32
rating_scale = 400
```

Game-specific values can be placed in the `GAME_CONFIG` table.

This is not a cryptographic anti-cheat system. It verifies reciprocal result claims, but two cooperating clients could still submit a false matching result unless a separate authoritative replay verification system is added.

### 2.5 Authoritative 8-player lobbies

Added `nakama/modules/ikemen_lobby.lua`.

A lobby supports up to eight players and keeps authoritative state for:

- Player membership/order.
- Lobby name.
- Game/build identity.
- Lobby format.
- Rules metadata.
- Current pairing.
- Tournament state.
- Result progression.

Supported scheduling formats are:

```text
winner_stays_on
round_robin
bracket
```

The lobby coordinates pairings but does not simulate the fight.

### 2.6 Lobby browser

The Lua API exposes `nakama.listLobbies()` through Nakama's match listing API.

The lobby browser is intentionally not implemented as a separate hardcoded renderer. Screenpacks/game scripts can use the returned lobby data to construct their own interface using normal IKEMEN Lua/UI facilities.

### 2.7 Nakama chat

Chat access is provided through the Nakama client:

```lua
nakama.joinChat(target)
nakama.sendChat(channel_id, message)
```

Incoming chat events are exposed to Lua through the Nakama event callback system.

### 2.8 P2P / NAT traversal primitive

Added `src/nakama_p2p.go`.

The current implementation provides:

- Local IPv4 candidate discovery.
- STUN Binding Request support.
- Server-reflexive address discovery.
- UDP hole-punching.
- Shared handshake token validation.
- Persistent UDP socket retention after the P2P connection succeeds.

The P2P object exposes the established UDP connection and remote endpoint so it can eventually be attached to the GGPO transport without closing/rebinding the NAT mapping.

Lua access is currently exposed as:

```lua
nakama.startP2P(port, stun_servers)
nakama.stopP2P()
```

The player is not expected to manually configure the connection method in the intended final flow.

## 3. Existing IKEMEN/GGPO Systems Retained

The implementation deliberately leaves the following existing mechanisms in place:

- IKEMEN deterministic game simulation.
- GGPO rollback simulation.
- Existing GGPO session/state logic.
- Existing UDP gameplay transport.
- Existing TCP-derived synchronization path.
- Existing replay file system.
- Existing Lua runtime.
- Existing screenpack/motif architecture.

The goal is to use Nakama to automate coordination rather than replace these systems unnecessarily.

## 4. Replay and Spectator System

### 4.1 Ten-second delayed spectator model

The default spectator publication delay is configured in `src/resources/defaultConfig.ini`:

```ini
Rollback.ReplayBroadcastDelay = 10
```

The replay stream operates at 60 frames per second, so the default delay is:

```text
60 FPS × 10 seconds = 600 frames
```

This delay is substantially larger than IKEMEN GO's rollback correction window and provides a buffer before replay input is exposed to spectators.

### 4.2 Existing replay representation reused

The spectator stream reuses the existing deterministic IKEMEN replay input representation instead of introducing a video stream or a separate game-state replication protocol.

The stream is represented by:

- replay header
- input frame chunks
- replay end message
- replay reset message

Replay chunks are currently 30 frames each.

The existing input representation is reused at eight controller slots and eight bytes of input data per controller per frame.

### 4.3 Rollback-aware publication

`src/replay_stream.go` implements `RollbackReplayStream`.

The rollback session invokes it from the existing replay recording path in `src/rollback.go`.

The stream:

1. Records inputs from the existing rollback timeline.
2. Waits for the configured publication delay.
3. Publishes only input frames that are old enough to be considered settled.
4. Tracks replay truncation.
5. Sends a reset when a rollback reaches data that has already been considered published.

This is intended to prevent speculative rollback data from becoming permanent spectator data.

### 4.4 Spectator-side buffering

`NakamaReplayBuffer` receives and retains replay stream data on the client.

It tracks:

- replay header
- contiguous buffered frame range
- final frame
- reset reason
- individual frame inputs

It can wait for a particular frame to arrive before replay playback consumes it.

### 4.5 Native replay playback

`src/netplay.go` extends `ReplayFile` with a live-buffer mode.

`NewLiveReplayFile()` wraps a `NakamaReplayBuffer` as a `ReplayFile` source.

`ReplayFile.Synchronize()` and `ReplayFile.Update()` recognize live replay buffers and feed the buffered deterministic inputs through the same replay playback path used for ordinary replay files.

The intent is therefore:

```text
Nakama replay chunks
        ↓
NakamaReplayBuffer
        ↓
NewLiveReplayFile
        ↓
existing ReplayFile playback
        ↓
IKEMEN deterministic simulation
```

No separate spectator renderer or video decoder is used.

## 5. Spectator Loading Screen

The ten-second spectator delay is treated as a loading/buffering period instead of showing the underlying game immediately.

The engine tracks the loading state in `System` and waits for the delayed replay buffer to reach the configured delay before beginning replay playback.

The engine-side loading state is exposed through:

```lua
motifState("nakama_spectator_loading")
```

The current implementation also exposes replay status through:

```lua
nakama.replayStatus()
```

including values such as:

- `buffered_through`
- `delay_frames`
- `ready`
- `loading`
- `frame_rate`
- `final_frame`
- `ended`
- `reset_reason`

## 6. Screenpack Integration

Nakama UI that is rendered by the engine is integrated into the motif/screenpack system rather than using fixed engine-only presentation values.

Added to `src/motif.go`:

```text
NakamaInfoProperties
NakamaInfo
NakamaBgDef
```

Added to `src/resources/defaultMotif.ini`:

```ini
[Nakama Info]
[NakamaBgDef]
```

### 6.1 Configurable Nakama loading presentation

The `[Nakama Info]` section provides screenpack-controlled properties for:

- title text
- loading text
- progress text
- remaining-time/detail text
- fonts
- offsets
- scales
- facing
- layers
- windows
- localcoord
- projection parameters
- fade properties
- status text
- error text

Example:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING
```

### 6.2 Configurable background

`[NakamaBgDef]` can provide a dedicated background definition for Nakama screens.

Existing screenpacks remain compatible because the spectator loading text can fall back to the existing `[Title Info]` loading/wait motif when the Nakama-specific element is disabled or unavailable.

### 6.3 Lua-driven screens

Lobby browser, lobby player list, tournament bracket, chat, online status, matchmaking, and similar screens are not hardcoded as a second menu system.

They are intended to be built by the screenpack/game's Lua code from Nakama data and events.

This keeps screenpack design responsibility in the same place as other IKEMEN menus rather than forcing one engine-defined visual style.

## 7. Lua API Added

`src/nakama_script.go` registers a global `nakama` table during `systemScriptInit()`.

Current functions include:

```lua
nakama.connect(config)
nakama.disconnect()
nakama.on(event, callback)

nakama.matchmake(mode, elo, elo_range, game, build, region)
nakama.cancelMatchmaking()

nakama.joinMatch(match_id, token)
nakama.leaveMatch()

nakama.startP2P(port, stun_servers)
nakama.stopP2P()

nakama.replayStatus()
nakama.startSpectatorReplay()

nakama.currentMatch()
nakama.userId()
nakama.username()

nakama.sendMatchData(opcode, data, reliable)
nakama.sendMatchDataBase64(opcode, base64, reliable)

nakama.joinChat(target, channel_type, persistence, hidden)
nakama.sendChat(channel_id, message)

nakama.listLobbies(query, label, min_size, max_size, limit)
nakama.createLobby(config)
nakama.reportLobbyResult(winner_id, loser_id)

nakama.submitResult(match_id, opponent_id, game, result, replay_hash)
```

## 8. Lua Event Model

The Nakama client parses realtime messages off-thread.

Lua callbacks are not invoked directly from the Nakama WebSocket goroutine.

Instead the implementation queues callback work onto IKEMEN's main-thread task mechanism.

The general flow is:

```text
Nakama WebSocket goroutine
        ↓
parse event
        ↓
IKEMEN mainThreadTask queue
        ↓
Lua callback
```

This avoids directly touching the gopher-lua state from a background network goroutine.

The P2P signaling path is also handled separately enough that the P2P handshake does not depend on Lua callbacks running while the game thread is waiting for synchronization.

## 9. Server Modules

### `nakama/modules/ikemen_matchmaker.lua`

Handles:

- ranked/unranked ticket processing
- game/build/region buckets
- server-side Elo lookup
- ranked opponent selection
- creation of `ikemen_session` matches

### `nakama/modules/ikemen_session.lua`

Acts as the Nakama coordination match for a matched 1v1 session.

Handles:

- expected players
- membership
- match data relay
- P2P signaling messages
- replay stream messages

It does not simulate IKEMEN gameplay.

### `nakama/modules/ikemen_lobby.lua`

Handles:

- lobby membership
- maximum eight players
- player ordering
- winner-stays-on scheduling
- round-robin scheduling
- bracket scheduling
- current pairing
- result progression
- lobby state broadcasts

### `nakama/modules/ikemen_rating.lua`

Handles:

- server-side rating storage
- Elo calculation
- reciprocal result validation
- duplicate settlement protection
- optional replay hash agreement

## 10. Code Changes by File

### Added files

| File | Purpose |
|---|---|
| `src/nakama_client.go` | Nakama REST/realtime client, matchmaking, lobby access, chat, signaling, replay transport hooks. |
| `src/nakama_p2p.go` | STUN, candidate gathering, UDP hole punching, P2P connection lifecycle. |
| `src/nakama_script.go` | Lua bindings and Lua event dispatch for Nakama functionality. |
| `src/replay_stream.go` | Rollback-aware delayed replay stream and client-side replay buffer. |
| `nakama/modules/ikemen_matchmaker.lua` | Ranked/unranked Nakama matchmaking processor. |
| `nakama/modules/ikemen_session.lua` | Coordination match for matched players and stream/signaling messages. |
| `nakama/modules/ikemen_lobby.lua` | Authoritative eight-player lobby and tournament scheduler. |
| `nakama/modules/ikemen_rating.lua` | Server-side Elo storage and result settlement. |
| `nakama/examples/ikemen_nakama.lua` | Example Lua integration/event registration. |
| `nakama/README.md` | Integration notes and usage documentation. |

### Modified files

| File | Changes |
|---|---|
| `src/rollback.go` | Connects the rollback session to the delayed replay stream, records frames when replay streaming is enabled, propagates replay truncation, ends the stream at match completion, and initializes the stream from configuration. |
| `src/netplay.go` | Stores synchronized replay metadata and exposes `ReplayFile` live-buffer mode for deterministic spectator playback. |
| `src/system.go` | Adds Nakama client state, spectator loading state, loading-screen rendering, delayed replay startup, and Nakama state rendering. |
| `src/motif.go` | Adds Nakama motif properties and optional Nakama background definition. |
| `src/resources/defaultMotif.ini` | Adds default Nakama screenpack configuration. |
| `src/resources/defaultConfig.ini` | Adds `Rollback.ReplayBroadcastDelay = 10`. |
| `src/script.go` | Registers the Nakama Lua API and exposes Nakama-related motif states. |
| `go.mod` | Pass 5 changed the Go language version from `1.27.0` to `1.23.2` as a local environment workaround. This is not the intended project requirement and should be restored to the project's required Go version for normal builds. |

## 11. Existing Network Transport and Current Integration Boundary

The implementation intentionally did not replace the external GGPO package's transport in the supplied tree.

The current IKEMEN rollback setup still uses the existing GGPO construction path and its existing UDP transport.

The new `NakamaP2P` object can establish and retain a usable UDP socket and exposes:

```text
UDPConn()
RemoteAddr()
```

The intended final direct-connection integration is:

```text
Nakama match
    ↓
P2P signaling
    ↓
STUN / UDP hole punch
    ↓
retained UDP socket
    ↓
existing GGPO transport boundary
    ↓
existing GGPO rollback session
```

The current source has not yet completed the last step of passing the already-established socket directly into the external GGPO transport constructor.

For this reason, the P2P code currently represents a connection-establishment primitive rather than a finished replacement for IKEMEN's existing gameplay transport.

## 12. Result Reporting and Ranked Integrity

The client can submit match results and a replay hash.

The Nakama rating module waits for reciprocal claims and requires:

- matching match identity
- matching opponent identity
- matching game identity
- complementary results
- matching replay hashes when both clients provide hashes

When those conditions are met, Elo is updated for both players.

This prevents simple one-sided accidental result updates and duplicate settlement, but it does not independently prove that the reported replay or result is truthful.

A future authoritative deterministic verifier could run the final replay against a compatible headless IKEMEN build before accepting ranked results. That verifier is not implemented in the current tree.

## 13. Compatibility / Sync Considerations

The existing IKEMEN synchronized replay header carries sync settings and a content fingerprint field.

However, the current engine implementation still contains a `currentContentFingerprint()` TODO and currently returns an empty string.

For production ranked matching, the fingerprint should be made meaningful and should cover all simulation-relevant content/build inputs before treating it as a reliable compatibility gate.

The matchmaking identity already carries game/build information so incompatible client builds can be separated at the Nakama layer.

## 14. Current Limitations

The current Pass 5 tree is an integration prototype rather than a finished production service.

The remaining major work is:

1. **Attach the retained NAT-traversed UDP socket to the external GGPO transport.**
   The current code stops at the transport handoff boundary.

2. **Automatic fallback behavior for networks where direct traversal fails.**
   A TURN/relay fallback has not been implemented.

3. **Fully generic live spectator initialization.**
   The deterministic input buffer and live `ReplayFile` support are present, but arbitrary game-specific replay initialization still depends on the game's setup/Lua path.

4. **New spectator joins.**
   The current documentation/implementation assumes a lobby member can receive the stream from its beginning. Historical replay backfill for a user who joins after the match has already started is not yet a complete protocol.

5. **Authoritative ranked replay verification.**
   Reciprocal claims and replay hashes are not a replacement for deterministic server verification.

6. **Production Nakama deployment configuration.**
   The Lua modules are supplied as integration modules; production deployment still requires configuration of the actual Nakama server, authentication, storage, runtime module loading, and deployment-specific STUN/relay infrastructure.

## 15. Validation Status

The supplied Pass 5 source tree was compared against the original IKEMEN GO source tree supplied for the project.

Pass 5 adds approximately 3,700 lines of new code/configuration and modifies the existing engine in a relatively small number of locations. Most of the new functionality lives in new files or Nakama modules rather than invasive changes to core combat/simulation code.

Static source parsing/gofmt checks were performed on the modified Go sources during development.

A complete engine build was not established against the original project's declared Go requirement in this environment. Pass 5 temporarily changed the `go` directive in `go.mod` to `1.23.2` to match the available local toolchain; this should not be treated as the correct project toolchain requirement. A Go 1.27.1 source archive was subsequently supplied for the environment, but the Pass 5 tree was not rebuilt and repackaged after switching toolchains.

## 16. Design Principle

The implementation is intentionally structured around the smallest useful bridge between Nakama and IKEMEN GO:

```text
Nakama:
    identity
    matchmaking
    ratings
    lobbies
    tournaments
    chat
    signaling
    delayed replay distribution

IKEMEN:
    simulation
    GGPO rollback
    UDP gameplay
    replay playback
    Lua
    screenpacks
```

The objective is to add online-service functionality without creating a second fighting-game networking stack, a second replay system, or a second screen/menu framework when the existing IKEMEN systems can perform the required job.


## Pass 6 continuation — recovered work and current delta

Pass 6 continues directly from the supplied Pass 5 tree. It does not replace the existing IKEMEN/GGPO simulation architecture.

### Recovered Pass 5 work preserved

The supplied Pass 5 tree already contains the Nakama client, ranked/unranked matchmaking, server-side Elo module, authoritative lobbies, chat, P2P/STUN/hole-punching, delayed replay streaming, spectator loading presentation, Lua bindings, replay synchronization, and the synchronization/rollback changes described above. Those systems remain the baseline.

### Pass 6 changes

1. `NakamaReplayBuffer.ApplyChunk` now advances the contiguous-frame boundary after every chunk. Previously the advancement path was limited to an initial-frame case, which could leave `BufferedThrough` stalled after the first chunk.

2. `NakamaReplayBuffer.Reset` now clears the received replay header as well as frames and end-state data. A reset therefore requires a new replay header before spectator startup can proceed.

3. The Nakama client can transfer ownership of an established `NakamaP2P` UDP socket through `TakeP2PTransport`. The transfer removes the socket from the P2P object's ownership before it is closed.

4. Rollback startup now prefers the retained Nakama UDP socket when the client is a member of a Nakama match and P2P is ready. It verifies that the P2P socket uses the configured `Rollback.Port`, obtains the actual observed remote UDP endpoint, and passes the retained socket to the GGPO transport.

5. `RollbackSession.InitP1` and `InitP2` accept an optional pre-bound UDP socket. Legacy GGPO socket creation remains the fallback when no pre-bound socket is supplied.

6. The spectator loading renderer no longer resets animated text every frame. Text sprites are updated once per frame, and Nakama-specific loading text falls back to the established title loading/wait text when the Nakama text is empty.

7. Strict synchronization validation now rejects a strict-setting schema length/path mismatch before value comparison. This closes the missing-field compatibility hole in the earlier strict comparison.

8. Realtime Lua callbacks are explicitly marshalled onto the engine main-thread task queue instead of invoking gopher-lua from a network goroutine. The queue is best-effort and a saturated queue drops the event rather than blocking the networking goroutine. Callback storage is local to the Lua setup instead of being held in a package-global map.

9. `JoinMatchWithMetadata` supplies the configured game/build identity when the caller does not provide them and no longer mutates the caller's metadata map.

10. Nakama Lua server modules now explicitly import `require("nakama")`, and `nakama/modules/main.lua` registers the authoritative `ikemen_session` and `ikemen_lobby` handlers while loading the matchmaker and rating modules.

11. Lobby join validation now requires game/build identity to match the lobby whenever those lobby identities are populated.

12. The lobby bracket scheduler now consumes automatic byes and advances to the next real match instead of leaving a `player2=nil` pairing active.

13. Nakama `dispatcher.broadcast_message` calls were normalized to the documented four-argument form.

14. `RollbackReplayStream.End` now flushes all remaining authoritative replay frames before publishing the final-frame marker. This closes a short-match/early-end case where the normal ten-second publication window had not elapsed before the fight ended.

15. `currentContentFingerprint()` now produces a deterministic SHA-256 digest from the engine version, active motif/fight-screen definitions, loaded character definitions and character FightFX, the active stage and attached-character definitions, and loaded common FightFX. The digest uses role labels and file contents rather than absolute installation paths.

### External GGPO boundary

The Pass 6 source now contains the IKEMEN-side transport handoff. The remaining dependency-side change is intentionally small: the pinned `github.com/ikemen-engine/ggpo` transport package must expose a constructor that wraps an already-open `net.PacketConn` without binding a second UDP socket. The implementation requirement is documented in `patches/ggpo-nakama-transport.md`.

The dependency is not vendored in the supplied repository and could not be fetched in this environment, so the constructor itself has not been compiled against the exact pinned GGPO source here.

### Ranked-result integrity correction

The earlier Pass 5 description of duplicate settlement protection was stronger than the implementation supports. The current Elo module stores a settlement marker per submitting user. Two reciprocal RPCs can race before either user's marker exists, so the marker is not an atomic match-wide claim. The implementation should therefore be treated as reciprocal-claim reconciliation, not complete duplicate-settlement protection, until settlement is moved into a single authoritative match path or a verified atomic compare-and-swap/transactional storage mechanism is used.

### Validation state

The project `go.mod` is restored to `go 1.27.0`. The available local compiler is Go 1.23.2, and the exact pinned GGPO source is not present in the local module cache. A full engine test was attempted with `GOTOOLCHAIN=local go test ./src` and is blocked immediately because the local toolchain is below the module's declared minimum. Consequently a full engine build has not been claimed.

Focused validation completed successfully:

- standalone P2P ownership tests: `go test ./...` → pass
- standalone replay-buffer/replay-stream tests, including final-frame flushing: `go test ./...` → pass
- Lua syntax checks for all Nakama modules and the example: all pass

### Recovered continuation state

This pass was reconstructed from the supplied Pass 5 source tree plus the later implementation material available in the project. The source tree, rather than an unavailable prior-chat transcript, is the authoritative record for code that is claimed as implemented here.

The next dependency-side blocker remains the pinned GGPO transport adapter. The content fingerprint is now meaningful and hashes the engine version plus active match definitions and loaded FightFX, but it is intentionally not claimed as an exhaustive digest of every dynamically loaded asset or runtime Lua file.


## Appendix B — Pass 7 historical record


## 1. Current architecture

Nakama supplies identity, matchmaking, ratings, lobbies, tournaments, chat,
signaling, and delayed replay distribution. IKEMEN GO remains responsible for
deterministic simulation, GGPO rollback, gameplay transport, replay playback,
Lua scripting, and screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket
is transferred into GGPO without rebinding so that the NAT traversal state is
preserved.

## 2. Recovered Pass 5/6 implementation

The current tree retains the previously completed work for:

- synchronized configuration schemas and session overrides;
- rollback UDP-port negotiation and compatibility checks;
- replay synchronization headers and restoration;
- content/build fingerprinting;
- Nakama client, matchmaking, lobby, chat and result APIs;
- Nakama Lua server modules and `modules/main.lua` registration;
- Lua-thread-safe event dispatch;
- STUN candidate gathering and UDP hole punching;
- delayed spectator replay buffering and loading presentation;
- replay final-frame flushing;
- Lua bindings for Nakama functionality.

## 3. Pass 7 changes

### 3.1 Bundled GGPO transport adapter

The exact pinned GGPO source is bundled under `third_party/ggpo` and the root
`go.mod` replaces the remote module with that local copy for reproducible builds.

Added:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

The constructor uses the existing GGPO `Udp` implementation and sender/receiver
lifecycle while adopting an already-bound `net.PacketConn`. It does not create a
second socket. The existing `NewUdp` constructor delegates to the same internal
initializer after binding its own socket.

### 3.2 Nakama P2P self-signal protection

Nakama match broadcasts can be delivered to the publishing client. The P2P
signal handler now ignores a signal whose token is the local token instead of
adopting the local socket as the remote peer.

### 3.3 Focused P2P regression coverage

Added engine-side tests for:

- ignoring self-originated P2P signals;
- completing a two-peer localhost UDP handshake;
- recording the actual observed remote endpoint for each peer.

## 4. GGPO verification

The bundled GGPO transport package passes its lifecycle tests, including the new
`NewUdpFromPacketConn` test covering socket reuse, unchanged local port, packet
receipt, close/unblock, and subsequent port rebind.

The GGPO core packages compile in an isolated validation copy. Existing upstream
GGPO tests unrelated to the transport adapter have pre-existing behavioral
failures in that validation environment; those failures are not represented as
transport-adapter failures.

## 5. Remaining validation boundary

A complete IKEMEN engine build still requires an actual Go 1.27.x compiler and the
project's native/build dependencies. The supplied Go 1.27.1 source archive is not
itself a compiled bootstrap toolchain, and the environment lacks the required
Go 1.24.6-or-newer bootstrap compiler.

The next real acceptance test is an end-to-end two-client match using a real
Nakama server and the bundled GGPO transport:

```text
Nakama authentication
    -> matchmaker
    -> ikemen_session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO transport
    -> rollback fight
    -> replay publication
```

## 6. Known limitations

TURN/relay fallback is not implemented. Late-joining spectator backfill is not
a complete protocol. Ranked-result handling still uses reciprocal claims and is
not an authoritative deterministic replay verifier. The content fingerprint
covers the configured active match/build definitions and loaded FightFX assets,
but it is not claimed to be an exhaustive digest of every dynamically loaded
runtime asset or Lua module.

The Elo settlement path remains non-atomic: two reciprocal submissions can race
before a match-wide settlement marker exists. It should therefore not be treated
as complete duplicate-settlement protection for a public ranked service.

## 7. Build integration

The development tree contains the GGPO source at `third_party/ggpo` and uses:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The standalone dependency patch is also supplied at:

```text
patches/ggpo-nakama-transport.patch
```

so the same change can be applied to the upstream GGPO module instead of using
the bundled source when the dependency is maintained externally.


# Pass 9 — Online Rematch / Session Transition

## Scope

Pass 9 integrates Kamekaze's IKEMEN GO rematch module with the existing Nakama coordination session and GGPO/P2P lifecycle. The goal is to make the post-match decision deterministic across both peers without replacing GGPO or creating a new Nakama coordination match for every rematch.

## 1. Online decision protocol

The existing Nakama `ikemen_session` coordination match now recognizes two opcodes:

- `OP_REMATCH_CHOICE = 1229673027`
- `OP_REMATCH_DECISION = 1229673028`

Each client sends `{version, action, round, snapshot}` using `nakama.rematchChoice()`. Valid actions are `rematch`, `select`, and `exit`. The Nakama match broadcasts one authoritative decision to both players.

Decision semantics:

- Either `exit` -> both peers exit.
- Otherwise either `select` -> both peers return to character select.
- Only when both choose `rematch` -> both peers start the next game without VS/order selection.

The coordination match stays alive across these transitions. This is a new gameplay/rollback session, not a second persistent Nakama matchmaking session.

## 2. Versus snapshot

`start.f_captureOnlineRematchSnapshot()` records the resolved post-VS state:

- stage number;
- P1/P2 team mode;
- ordered character references;
- selected palettes.

The capture runs after the online versus screen, including order selection. Numeric character references are accepted by `launchFight()` so the rematch does not depend on character-name lookup or local ordering.

The server validates snapshot structure, positive integer stage/character/palette values, and team-mode fields. When both players choose rematch their snapshots must agree; a mismatch becomes a Character Select decision instead of allowing one peer to impose a different game state.

## 3. GGPO/P2P lifecycle

A major lifecycle constraint is that the current GGPO backend owns the active UDP socket after the Nakama P2P socket has been transferred. The next P2P socket therefore cannot be created while the current `game()` call is still active.

Pass 9 moves P2P startup to the mode loop after the current gameplay session has returned through `start.f_game()`. This guarantees that rollback teardown has already closed the previous GGPO socket.

Character Select starts a fresh P2P socket before entering selection and waits for it to become ready after selection finishes. A rematch starts a fresh P2P socket after the previous match and waits for readiness before launching the forced character/stage fight.

## 4. Rematch launch

Rematches bypass VS and order select with:

```text
onlineSession = true
vsscreen = false
p1orderselect = false
p2orderselect = false
continue = false
quickcontinue = true
stageNo = saved stage
forceStage = true
p1char / p2char = saved teams
```

`start.f_setStage()` gained a distinct `forceStage` argument so `main.stageMenu` cannot replace the negotiated stage. This is intentionally separate from ordinary `assigned` stage behavior and does not change continued-match stage selection.

## 5. Ranked side-switch interaction

Ranked side switching is applied by `start.f_game()` after the current `game()` call records the winner. A VS snapshot is physically side-oriented, so `start.f_prepareOnlineRematchSnapshot()` swaps the two sides of the snapshot after a non-final ranked match when `switch_sides` is enabled. This keeps each stable set player associated with the side assigned by `RankedSet`.

Character Select already uses the ranked carry-selection mechanism, so the same side assignment applies to the select path.

## 6. Ranked final-match behavior

Before showing an online rematch menu, `ik_rematch.lua` checks the current ranked-set state and current winner. If the current match supplies the required majority for the negotiated best-of-N set, the rematch menu is suppressed and `start.onlineNextAction` is set to `exit`. `endMatch()` returns control to `start.f_game()`, which records the final set result and then returns through the normal online-mode exit path.

This prevents a post-final-match rematch from bypassing `RankedSet.RecordMatch()`.

## 7. Online UI state

The Kamekaze module keeps its original offline two-choice behavior intact. Nakama online mode has a separate three-choice UI: Rematch, Character Select, Exit, with a waiting state after a local choice is submitted. Server decisions are applied on the common game loop so both peers take the same transition.

The module temporarily disables the local Kamekaze Yes/No override only while the Nakama online rematch flow is active and restores the previous override value when online mode ends.

## 8. Exit path

Exit does not leave Nakama from inside the live gameplay callback. The active gameplay session is first terminated, allowing normal post-match bookkeeping to complete. `start.f_selectMode()` then stops/leaves the Nakama coordination session and returns from `netplayversus` to its existing caller/menu.

## 9. Correctness fixes

Pass 9 corrects several issues discovered during integration:

- removed premature P2P restart from the rematch callback;
- removed premature Nakama match leave from the exit callback;
- prevented a final ranked match from presenting a rematch menu;
- preserved user state for `ik_rematch.override`;
- forced the negotiated rematch stage despite `main.stageMenu`;
- preserved ranked player identity across side-switching rematches;
- rejected malformed and mismatched rematch snapshots server-side.

## 10. Validation status

The source-level integration is complete. The existing focused P2P, GGPO transport, replay, and ranked-set tests remain part of the tree. Full IKEMEN compilation is still dependent on a usable Go 1.27.x toolchain and target-platform native dependencies in the build environment.

Pass 9 has not been represented as a production-ranked deployment. Atomic/authoritative result settlement, deterministic replay verification, and TURN fallback remain separate production concerns.

## 11. Files changed in Pass 9

```text
external/mods/ik_rematch.lua
external/mods/rematch.def
external/script/start.lua
nakama/modules/ikemen_session.lua
nakama/README.md
IKEMEN-GO-Nakama-Implementation-pass9.md
```

The underlying Pass 6–8 implementation records remain preserved in the repository. This Pass 9 record is the current continuation record.


---

# Pass 10

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass10.md`

# Current Pass 10 Status

This document preserves the earlier Pass 6–8 implementation history below and then records the Pass 9 and Pass 10 continuations at the end. Statements in the historical sections describing missing GGPO transport support refer to the state at that earlier pass; the current tree now bundles the modified GGPO dependency under `third_party/ggpo`.

Current Pass 10 additions extend the Nakama-coordinated online rematch flow into ranked best-of-X sets with a three-choice non-final prompt (Rematch / Character Select / Forfeit) and a two-choice final-set prompt (Play Another Set / Exit). Pass 10 also adds ranked-set forfeit/reset semantics, server-side validation of final-set actions, and a fresh-set transition back to Character Select.

# IKEMEN GO + Nakama — Consolidated Implementation Record

## 1. Record scope

This file is the consolidated implementation record for the current development tree. It carries the earlier Pass 6–8 implementation history and the Pass 9–10 continuations so the current source state can be reconstructed from one record. The source tree is authoritative for what is actually present in code.

## 2. Current architecture

Nakama provides account identity, matchmaking, lobby/presence, chat, signaling, ratings/result plumbing, and delayed replay distribution. IKEMEN GO remains responsible for deterministic simulation, GGPO rollback, gameplay transport, replay playback, Lua execution, renderer composition, and game/screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket is transferred into GGPO without rebinding so that the NAT traversal state is preserved.

The optional border/presentation system is render-only. It provides a physical outer canvas, independently configured game viewport, border-authored assets, and Lua render hooks. It does not participate in rollback state or deterministic simulation.

Ranked sets are session/presentation state. Best-of-X, side switching, and winner-selection locking are negotiated outside rollback and propagated through Nakama matchmaking/session metadata.

## 3. Completed implementation through Pass 8

### 3.1 Synchronization and session overrides

The tree contains synchronized configuration schemas, strict/host scopes, deterministic serialization/validation, session-wide overrides, restoration, synchronization warnings, effective fight-aspect compatibility checks, replay synchronization headers, rollback UDP-port negotiation, and content/build fingerprint plumbing.

### 3.2 Nakama integration

The tree contains the Nakama client, matchmaking, lobby/presence, chat, result plumbing, server Lua modules, `modules/main.lua` registration, Lua-thread-safe event dispatch, STUN candidate gathering, UDP hole punching, P2P signaling, delayed spectator replay loading, and Lua bindings for Nakama/session information.

Nakama match broadcasting is protected against accepting the local client's own P2P signal.

### 3.3 GGPO transport integration

The pinned GGPO source is bundled in `third_party/ggpo` and the root module uses a local replacement:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The GGPO transport exposes:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

This adopts the already-bound Nakama/STUN `PacketConn` without creating a second UDP socket. The existing GGPO constructor delegates to the same initializer after binding its own socket.

The transport lifecycle test verifies socket reuse, unchanged local port, packet receipt, close/unblock, and subsequent port rebind.

### 3.4 Presentation canvas / border system

The engine now distinguishes the physical presentation canvas from the game viewport.

When border presentation is enabled:

```text
outer canvas = Video.Border.Width  × Video.Border.Height
game viewport = Video.GameWidth     × Video.GameHeight
```

The game viewport is centered inside the border by default. Signed `Video.Border.GameOffsetX` and `Video.Border.GameOffsetY` move it relative to that centered position.

Command-line controls:

```text
-bordered
-borderwidth <pixels>
-borderheight <pixels>
-bordergamex <pixels>
-bordergamey <pixels>
```

Normal non-bordered resolutions continue to use the game resolution as the output resolution. Bordered resolution presets bind an outer resolution to an independent game resolution. Current defaults include:

```text
1280x720  -> 960x720
1600x900  -> 1200x900
1920x1080 -> 1440x1080
1920x1200 -> 1600x1200
2560x1440 -> 1920x1440
```

The options system has separate `Resolution` and `Bordered Resolutions` menus. Bordered presets enable presentation and reset placement offsets. Custom bordered resolution accepts outer width/height and game width/height independently.

The renderer paths were updated for OpenGL 3.3, OpenGL ES 3.2, and Vulkan. Font rendering uses the active presentation viewport. Scissor rectangles account for the presentation origin.

### 3.5 Border authoring

The motif now contains a `[Border]` section with separate background and foreground layers. Each layer uses the engine's existing animation, text, and rectangle primitives.

The intended syntax is:

```text
[Border]
enabled = 1
localcoord = 1920, 1080
background.anim = ...
background.text.text = ...
background.overlay.col = ...
background.overlay.alpha = ...
foreground.anim = ...
foreground.text.text = ...
foreground.overlay.col = ...
foreground.overlay.alpha = ...
```

`BorderLayerProperties` uses the existing `AnimationProperties` directly plus grouped `text` and `overlay` sections. The border's own `localcoord` is the inherited coordinate space for child animation/text/overlay data unless a child explicitly supplies its own localcoord.

The border layer updates animation, text, and rectangle state each render frame and renders across the existing layer numbers. Render hooks are:

```text
game.border_bg
game.border
```

They execute on the outer presentation canvas. Existing Lua animation, text, and rectangle APIs can therefore be used to construct dynamic side panels, player information, chat, spectator data, challenge displays, and other UI without coupling those elements to the rollback simulation.

Lua accessors include:

```text
borderEnabled()
borderWidth()
borderHeight()
borderGameX()
borderGameY()
borderGameWidth()
borderGameHeight()
```

The border is deliberately not implemented as a conventional OS window border. It is a composition surface around the gameplay viewport so game and border resolutions remain independently addressable.

### 3.6 Ranked best-of-X sets

`Netplay.Ranked` contains:

```text
BestOf
SwitchSides
WinnerKeepsSelection
```

`BestOf` is normalized to an odd value from 1 through 99.

The matchmaker includes the negotiated ranked rules in its compatibility bucket and session label so players with different set rules are not silently paired into one rule set.

A render/session-only `RankedSet` object tracks:

- stable player identities;
- current physical side assignment;
- wins and completed match count;
- set winner;
- immediate previous match winner;
- character/team selection tokens.

A set completes as soon as one player reaches the majority required by BestOf. When `SwitchSides` is enabled, physical side assignment swaps only after a non-terminal match. Local controller mappings are then swapped so player identity follows the assigned side.

When `WinnerKeepsSelection` is enabled, the previous match winner's confirmed character/team selection is carried into the next match after the side assignment is resolved. The loser remains selectable.

The selection carry is applied before the next team selection menu is constructed, and the finished set exits the repeated-match loop when a majority winner is reached.

### 3.7 Ranked Lua/session API

The Nakama Lua surface exposes ranked-set state and actions through:

```text
nakama.rankedSet()
nakama.rankedSetRecordMatch(winnerSide)
nakama.rankedSetSelectionLocked(player, token)
```

The state table includes set activity, BestOf, match count, win counts, player IDs, physical side mapping, switch-side policy, winner-selection policy, set winner, and previous match winner.

### 3.8 Replay and spectator behavior

Delayed replay distribution remains integrated with rollback. The replay buffer flushes final delayed frames before the live stream is terminated, including matches shorter than the normal spectator delay.

Spectator replay startup, loading UI, replay synchronization headers, and session configuration restoration remain part of the existing implementation.

### 3.9 Content fingerprint

The content fingerprint now produces a deterministic SHA-256 over the configured active match/build definitions and loaded FightFX assets. Machine-specific absolute paths are not used as the hash identity.

The implementation is intentionally not described as an exhaustive digest of every dynamically loaded runtime asset or Lua module; that remains a separate hardening task.

## 4. Corrective fixes made during Pass 8 continuation

### 4.1 Ranked-set reset state

`RankedSet.Reset()` now explicitly restores both `winner` and `lastWinner` to `-1`. This prevents a reset ranked session from exposing a stale previous-winner state through Lua.

A regression test covers the reset behavior.

### 4.2 Border motif field layout

The border animation definition is flattened to use the same convention as other motif animation properties (`background.anim`, etc.) while keeping text and overlay grouped under their respective keys.

### 4.3 Border localcoord inheritance

The data-pointer population path recognizes `BorderInfoProperties.Localcoord` as the coordinate-space root for border children. Explicit per-element localcoord values remain authoritative.

### 4.4 Border rectangle animation

Border overlay rectangles are updated every render frame so pulsing alpha and related rectangle state animate correctly.

### 4.5 Lua hook syntax correction

The presentation-hook documentation comment was corrected so it does not become part of a malformed Lua function declaration.

## 5. Validation performed

### Passed

- ranked-set standalone tests using the available Go 1.23 compiler:
  - BestOf normalization;
  - majority set completion;
  - side switching;
  - winner-selection lock;
  - reset clears previous winner.
- P2P self-signal rejection test.
- Two-peer localhost UDP punching/handshake test.
- GGPO `NewUdpFromPacketConn` lifecycle test.
- GGPO core package compilation in an isolated validation copy.
- Replay-buffer final-frame flush regression test.
- Source formatting with `gofmt` for modified Go files.

### Not yet fully validated

A complete IKEMEN engine build/test run has not been completed in this environment because the project requires Go 1.27.0+ and the local compiler is Go 1.23.2. The supplied Go 1.27.1 archive is source code and itself requires a Go 1.24.6-or-newer bootstrap compiler.

The environment also has no network access for automatic module/toolchain acquisition and no installed Lua interpreter for an independent Lua parser/runtime check.

## 6. Build status

The source tree is structured for the project's existing platform build scripts. The bundled GGPO dependency is part of the source tree and does not require a separately maintained fork at build time.

Builds still require the appropriate target toolchain and native dependencies described by `BUILDING.md` for Windows, Linux, macOS, and Android.

The current tree should therefore be treated as a development/test release candidate rather than as a fully cross-compiled and end-to-end validated public release.

## 7. Remaining engineering work

The major remaining online acceptance boundary is a real two-client session:

```text
Nakama authentication
    -> matchmaking
    -> session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO
    -> real rollback fight
    -> replay publication
```

Known limitations remain:

- TURN/relay fallback is not implemented;
- late-joining spectator backfill is incomplete;
- ranked result handling is not yet an authoritative deterministic replay verifier;
- Elo/result settlement is not atomic duplicate-settlement protection;
- content fingerprinting is not exhaustive of every dynamic runtime resource.

## 8. Pass 8 design rules

The border feature must remain presentation-only. Dynamic border information may include player data, online status, chat, spectators, lobby state, challenge state, and other non-deterministic UI information, but it must not directly mutate rollback gameplay state during a synchronized fight.

Border presentation should remain disabled without opt-in and must preserve ordinary non-bordered behavior when disabled.

Ranked set rules are session state, not rollback state. The game simulation remains governed by the existing deterministic synchronization and GGPO mechanisms.

## Appendix A — Pass 6 historical record


## 1. Overview

This project adds a Nakama-backed online service layer to IKEMEN GO while retaining IKEMEN GO's existing deterministic simulation, GGPO rollback system, UDP gameplay transport, replay system, Lua scripting model, and screenpack/motif system as the primary game-side machinery.

The intended architecture is:

```text
                         NAKAMA
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   Authentication      Matchmaking         Lobbies
        │                  │                  │
        │             Ranked/Unranked     Tournament
        │                  │              Scheduling
        │                  │                  │
        └──────────────────┼──────────────────┘
                           │
                     Match/session
                           │
                 P2P signaling / setup
                           │
              ┌────────────┴────────────┐
              │                         │
           Player A                  Player B
              │                         │
              └──── existing GGPO ─────┘
                           │
                   deterministic replay
                           │
                     600-frame delay
                           │
                           ▼
                        Nakama
                           │
                 delayed input stream
                           │
                   ┌───────┼───────┐
                   ▼       ▼       ▼
               spectator spectator spectator
                   │       │       │
                   └── IKEMEN deterministic playback ──┘
```

Nakama is intended to be the control plane and social/matchmaking service. It is not intended to become the live fighting-game simulation server.

## 2. Implemented Features

### 2.1 Nakama client integration

Added an engine-side Nakama client in `src/nakama_client.go`.

Implemented capabilities include:

- REST authentication using device/session credentials.
- Realtime WebSocket connection.
- WebSocket read/write loops.
- Event dispatch from Nakama into IKEMEN.
- Match join/leave.
- Match data send operations.
- Server RPC calls.
- Lobby listing through Nakama authoritative match listing.
- Chat channel join/send.
- P2P signaling messages.
- Replay header/chunk/end/reset messages.

The client is exposed to Lua through `src/nakama_script.go`.

### 2.2 Ranked matchmaking

The Nakama server module `nakama/modules/ikemen_matchmaker.lua` implements ranked and unranked matchmaking.

Ranked matchmaking:

- Groups tickets by game, build, mode, and region.
- Retrieves the player's rating from server-side storage.
- Compares rating distance against the configured allowed range.
- Chooses a compatible opponent.
- Creates a Nakama `ikemen_session` match for the two players.

The client can provide an Elo range as matchmaking metadata, but the authoritative rating is intended to come from server-side storage.

### 2.3 Unranked matchmaking

Unranked matchmaking uses the same matchmaker infrastructure while ignoring Elo distance when pairing compatible tickets.

The matchmaking target is two players.

### 2.4 Server-side Elo

Added `nakama/modules/ikemen_rating.lua`.

The module provides:

- Per-game rating storage.
- Configurable initial rating.
- Configurable K-factor.
- Configurable rating scale.
- Wins/losses/game counters.
- Reciprocal result submission.
- Match-specific settlement protection so the same match is not intended to change ratings twice.
- Optional replay hash agreement checking.

Default configuration currently is:

```lua
initial_rating = 1000
k_factor = 32
rating_scale = 400
```

Game-specific values can be placed in the `GAME_CONFIG` table.

This is not a cryptographic anti-cheat system. It verifies reciprocal result claims, but two cooperating clients could still submit a false matching result unless a separate authoritative replay verification system is added.

### 2.5 Authoritative 8-player lobbies

Added `nakama/modules/ikemen_lobby.lua`.

A lobby supports up to eight players and keeps authoritative state for:

- Player membership/order.
- Lobby name.
- Game/build identity.
- Lobby format.
- Rules metadata.
- Current pairing.
- Tournament state.
- Result progression.

Supported scheduling formats are:

```text
winner_stays_on
round_robin
bracket
```

The lobby coordinates pairings but does not simulate the fight.

### 2.6 Lobby browser

The Lua API exposes `nakama.listLobbies()` through Nakama's match listing API.

The lobby browser is intentionally not implemented as a separate hardcoded renderer. Screenpacks/game scripts can use the returned lobby data to construct their own interface using normal IKEMEN Lua/UI facilities.

### 2.7 Nakama chat

Chat access is provided through the Nakama client:

```lua
nakama.joinChat(target)
nakama.sendChat(channel_id, message)
```

Incoming chat events are exposed to Lua through the Nakama event callback system.

### 2.8 P2P / NAT traversal primitive

Added `src/nakama_p2p.go`.

The current implementation provides:

- Local IPv4 candidate discovery.
- STUN Binding Request support.
- Server-reflexive address discovery.
- UDP hole-punching.
- Shared handshake token validation.
- Persistent UDP socket retention after the P2P connection succeeds.

The P2P object exposes the established UDP connection and remote endpoint so it can eventually be attached to the GGPO transport without closing/rebinding the NAT mapping.

Lua access is currently exposed as:

```lua
nakama.startP2P(port, stun_servers)
nakama.stopP2P()
```

The player is not expected to manually configure the connection method in the intended final flow.

## 3. Existing IKEMEN/GGPO Systems Retained

The implementation deliberately leaves the following existing mechanisms in place:

- IKEMEN deterministic game simulation.
- GGPO rollback simulation.
- Existing GGPO session/state logic.
- Existing UDP gameplay transport.
- Existing TCP-derived synchronization path.
- Existing replay file system.
- Existing Lua runtime.
- Existing screenpack/motif architecture.

The goal is to use Nakama to automate coordination rather than replace these systems unnecessarily.

## 4. Replay and Spectator System

### 4.1 Ten-second delayed spectator model

The default spectator publication delay is configured in `src/resources/defaultConfig.ini`:

```ini
Rollback.ReplayBroadcastDelay = 10
```

The replay stream operates at 60 frames per second, so the default delay is:

```text
60 FPS × 10 seconds = 600 frames
```

This delay is substantially larger than IKEMEN GO's rollback correction window and provides a buffer before replay input is exposed to spectators.

### 4.2 Existing replay representation reused

The spectator stream reuses the existing deterministic IKEMEN replay input representation instead of introducing a video stream or a separate game-state replication protocol.

The stream is represented by:

- replay header
- input frame chunks
- replay end message
- replay reset message

Replay chunks are currently 30 frames each.

The existing input representation is reused at eight controller slots and eight bytes of input data per controller per frame.

### 4.3 Rollback-aware publication

`src/replay_stream.go` implements `RollbackReplayStream`.

The rollback session invokes it from the existing replay recording path in `src/rollback.go`.

The stream:

1. Records inputs from the existing rollback timeline.
2. Waits for the configured publication delay.
3. Publishes only input frames that are old enough to be considered settled.
4. Tracks replay truncation.
5. Sends a reset when a rollback reaches data that has already been considered published.

This is intended to prevent speculative rollback data from becoming permanent spectator data.

### 4.4 Spectator-side buffering

`NakamaReplayBuffer` receives and retains replay stream data on the client.

It tracks:

- replay header
- contiguous buffered frame range
- final frame
- reset reason
- individual frame inputs

It can wait for a particular frame to arrive before replay playback consumes it.

### 4.5 Native replay playback

`src/netplay.go` extends `ReplayFile` with a live-buffer mode.

`NewLiveReplayFile()` wraps a `NakamaReplayBuffer` as a `ReplayFile` source.

`ReplayFile.Synchronize()` and `ReplayFile.Update()` recognize live replay buffers and feed the buffered deterministic inputs through the same replay playback path used for ordinary replay files.

The intent is therefore:

```text
Nakama replay chunks
        ↓
NakamaReplayBuffer
        ↓
NewLiveReplayFile
        ↓
existing ReplayFile playback
        ↓
IKEMEN deterministic simulation
```

No separate spectator renderer or video decoder is used.

## 5. Spectator Loading Screen

The ten-second spectator delay is treated as a loading/buffering period instead of showing the underlying game immediately.

The engine tracks the loading state in `System` and waits for the delayed replay buffer to reach the configured delay before beginning replay playback.

The engine-side loading state is exposed through:

```lua
motifState("nakama_spectator_loading")
```

The current implementation also exposes replay status through:

```lua
nakama.replayStatus()
```

including values such as:

- `buffered_through`
- `delay_frames`
- `ready`
- `loading`
- `frame_rate`
- `final_frame`
- `ended`
- `reset_reason`

## 6. Screenpack Integration

Nakama UI that is rendered by the engine is integrated into the motif/screenpack system rather than using fixed engine-only presentation values.

Added to `src/motif.go`:

```text
NakamaInfoProperties
NakamaInfo
NakamaBgDef
```

Added to `src/resources/defaultMotif.ini`:

```ini
[Nakama Info]
[NakamaBgDef]
```

### 6.1 Configurable Nakama loading presentation

The `[Nakama Info]` section provides screenpack-controlled properties for:

- title text
- loading text
- progress text
- remaining-time/detail text
- fonts
- offsets
- scales
- facing
- layers
- windows
- localcoord
- projection parameters
- fade properties
- status text
- error text

Example:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING
```

### 6.2 Configurable background

`[NakamaBgDef]` can provide a dedicated background definition for Nakama screens.

Existing screenpacks remain compatible because the spectator loading text can fall back to the existing `[Title Info]` loading/wait motif when the Nakama-specific element is disabled or unavailable.

### 6.3 Lua-driven screens

Lobby browser, lobby player list, tournament bracket, chat, online status, matchmaking, and similar screens are not hardcoded as a second menu system.

They are intended to be built by the screenpack/game's Lua code from Nakama data and events.

This keeps screenpack design responsibility in the same place as other IKEMEN menus rather than forcing one engine-defined visual style.

## 7. Lua API Added

`src/nakama_script.go` registers a global `nakama` table during `systemScriptInit()`.

Current functions include:

```lua
nakama.connect(config)
nakama.disconnect()
nakama.on(event, callback)

nakama.matchmake(mode, elo, elo_range, game, build, region)
nakama.cancelMatchmaking()

nakama.joinMatch(match_id, token)
nakama.leaveMatch()

nakama.startP2P(port, stun_servers)
nakama.stopP2P()

nakama.replayStatus()
nakama.startSpectatorReplay()

nakama.currentMatch()
nakama.userId()
nakama.username()

nakama.sendMatchData(opcode, data, reliable)
nakama.sendMatchDataBase64(opcode, base64, reliable)

nakama.joinChat(target, channel_type, persistence, hidden)
nakama.sendChat(channel_id, message)

nakama.listLobbies(query, label, min_size, max_size, limit)
nakama.createLobby(config)
nakama.reportLobbyResult(winner_id, loser_id)

nakama.submitResult(match_id, opponent_id, game, result, replay_hash)
```

## 8. Lua Event Model

The Nakama client parses realtime messages off-thread.

Lua callbacks are not invoked directly from the Nakama WebSocket goroutine.

Instead the implementation queues callback work onto IKEMEN's main-thread task mechanism.

The general flow is:

```text
Nakama WebSocket goroutine
        ↓
parse event
        ↓
IKEMEN mainThreadTask queue
        ↓
Lua callback
```

This avoids directly touching the gopher-lua state from a background network goroutine.

The P2P signaling path is also handled separately enough that the P2P handshake does not depend on Lua callbacks running while the game thread is waiting for synchronization.

## 9. Server Modules

### `nakama/modules/ikemen_matchmaker.lua`

Handles:

- ranked/unranked ticket processing
- game/build/region buckets
- server-side Elo lookup
- ranked opponent selection
- creation of `ikemen_session` matches

### `nakama/modules/ikemen_session.lua`

Acts as the Nakama coordination match for a matched 1v1 session.

Handles:

- expected players
- membership
- match data relay
- P2P signaling messages
- replay stream messages

It does not simulate IKEMEN gameplay.

### `nakama/modules/ikemen_lobby.lua`

Handles:

- lobby membership
- maximum eight players
- player ordering
- winner-stays-on scheduling
- round-robin scheduling
- bracket scheduling
- current pairing
- result progression
- lobby state broadcasts

### `nakama/modules/ikemen_rating.lua`

Handles:

- server-side rating storage
- Elo calculation
- reciprocal result validation
- duplicate settlement protection
- optional replay hash agreement

## 10. Code Changes by File

### Added files

| File | Purpose |
|---|---|
| `src/nakama_client.go` | Nakama REST/realtime client, matchmaking, lobby access, chat, signaling, replay transport hooks. |
| `src/nakama_p2p.go` | STUN, candidate gathering, UDP hole punching, P2P connection lifecycle. |
| `src/nakama_script.go` | Lua bindings and Lua event dispatch for Nakama functionality. |
| `src/replay_stream.go` | Rollback-aware delayed replay stream and client-side replay buffer. |
| `nakama/modules/ikemen_matchmaker.lua` | Ranked/unranked Nakama matchmaking processor. |
| `nakama/modules/ikemen_session.lua` | Coordination match for matched players and stream/signaling messages. |
| `nakama/modules/ikemen_lobby.lua` | Authoritative eight-player lobby and tournament scheduler. |
| `nakama/modules/ikemen_rating.lua` | Server-side Elo storage and result settlement. |
| `nakama/examples/ikemen_nakama.lua` | Example Lua integration/event registration. |
| `nakama/README.md` | Integration notes and usage documentation. |

### Modified files

| File | Changes |
|---|---|
| `src/rollback.go` | Connects the rollback session to the delayed replay stream, records frames when replay streaming is enabled, propagates replay truncation, ends the stream at match completion, and initializes the stream from configuration. |
| `src/netplay.go` | Stores synchronized replay metadata and exposes `ReplayFile` live-buffer mode for deterministic spectator playback. |
| `src/system.go` | Adds Nakama client state, spectator loading state, loading-screen rendering, delayed replay startup, and Nakama state rendering. |
| `src/motif.go` | Adds Nakama motif properties and optional Nakama background definition. |
| `src/resources/defaultMotif.ini` | Adds default Nakama screenpack configuration. |
| `src/resources/defaultConfig.ini` | Adds `Rollback.ReplayBroadcastDelay = 10`. |
| `src/script.go` | Registers the Nakama Lua API and exposes Nakama-related motif states. |
| `go.mod` | Pass 5 changed the Go language version from `1.27.0` to `1.23.2` as a local environment workaround. This is not the intended project requirement and should be restored to the project's required Go version for normal builds. |

## 11. Existing Network Transport and Current Integration Boundary

The implementation intentionally did not replace the external GGPO package's transport in the supplied tree.

The current IKEMEN rollback setup still uses the existing GGPO construction path and its existing UDP transport.

The new `NakamaP2P` object can establish and retain a usable UDP socket and exposes:

```text
UDPConn()
RemoteAddr()
```

The intended final direct-connection integration is:

```text
Nakama match
    ↓
P2P signaling
    ↓
STUN / UDP hole punch
    ↓
retained UDP socket
    ↓
existing GGPO transport boundary
    ↓
existing GGPO rollback session
```

The current source has not yet completed the last step of passing the already-established socket directly into the external GGPO transport constructor.

For this reason, the P2P code currently represents a connection-establishment primitive rather than a finished replacement for IKEMEN's existing gameplay transport.

## 12. Result Reporting and Ranked Integrity

The client can submit match results and a replay hash.

The Nakama rating module waits for reciprocal claims and requires:

- matching match identity
- matching opponent identity
- matching game identity
- complementary results
- matching replay hashes when both clients provide hashes

When those conditions are met, Elo is updated for both players.

This prevents simple one-sided accidental result updates and duplicate settlement, but it does not independently prove that the reported replay or result is truthful.

A future authoritative deterministic verifier could run the final replay against a compatible headless IKEMEN build before accepting ranked results. That verifier is not implemented in the current tree.

## 13. Compatibility / Sync Considerations

The existing IKEMEN synchronized replay header carries sync settings and a content fingerprint field.

However, the current engine implementation still contains a `currentContentFingerprint()` TODO and currently returns an empty string.

For production ranked matching, the fingerprint should be made meaningful and should cover all simulation-relevant content/build inputs before treating it as a reliable compatibility gate.

The matchmaking identity already carries game/build information so incompatible client builds can be separated at the Nakama layer.

## 14. Current Limitations

The current Pass 5 tree is an integration prototype rather than a finished production service.

The remaining major work is:

1. **Attach the retained NAT-traversed UDP socket to the external GGPO transport.**
   The current code stops at the transport handoff boundary.

2. **Automatic fallback behavior for networks where direct traversal fails.**
   A TURN/relay fallback has not been implemented.

3. **Fully generic live spectator initialization.**
   The deterministic input buffer and live `ReplayFile` support are present, but arbitrary game-specific replay initialization still depends on the game's setup/Lua path.

4. **New spectator joins.**
   The current documentation/implementation assumes a lobby member can receive the stream from its beginning. Historical replay backfill for a user who joins after the match has already started is not yet a complete protocol.

5. **Authoritative ranked replay verification.**
   Reciprocal claims and replay hashes are not a replacement for deterministic server verification.

6. **Production Nakama deployment configuration.**
   The Lua modules are supplied as integration modules; production deployment still requires configuration of the actual Nakama server, authentication, storage, runtime module loading, and deployment-specific STUN/relay infrastructure.

## 15. Validation Status

The supplied Pass 5 source tree was compared against the original IKEMEN GO source tree supplied for the project.

Pass 5 adds approximately 3,700 lines of new code/configuration and modifies the existing engine in a relatively small number of locations. Most of the new functionality lives in new files or Nakama modules rather than invasive changes to core combat/simulation code.

Static source parsing/gofmt checks were performed on the modified Go sources during development.

A complete engine build was not established against the original project's declared Go requirement in this environment. Pass 5 temporarily changed the `go` directive in `go.mod` to `1.23.2` to match the available local toolchain; this should not be treated as the correct project toolchain requirement. A Go 1.27.1 source archive was subsequently supplied for the environment, but the Pass 5 tree was not rebuilt and repackaged after switching toolchains.

## 16. Design Principle

The implementation is intentionally structured around the smallest useful bridge between Nakama and IKEMEN GO:

```text
Nakama:
    identity
    matchmaking
    ratings
    lobbies
    tournaments
    chat
    signaling
    delayed replay distribution

IKEMEN:
    simulation
    GGPO rollback
    UDP gameplay
    replay playback
    Lua
    screenpacks
```

The objective is to add online-service functionality without creating a second fighting-game networking stack, a second replay system, or a second screen/menu framework when the existing IKEMEN systems can perform the required job.


## Pass 6 continuation — recovered work and current delta

Pass 6 continues directly from the supplied Pass 5 tree. It does not replace the existing IKEMEN/GGPO simulation architecture.

### Recovered Pass 5 work preserved

The supplied Pass 5 tree already contains the Nakama client, ranked/unranked matchmaking, server-side Elo module, authoritative lobbies, chat, P2P/STUN/hole-punching, delayed replay streaming, spectator loading presentation, Lua bindings, replay synchronization, and the synchronization/rollback changes described above. Those systems remain the baseline.

### Pass 6 changes

1. `NakamaReplayBuffer.ApplyChunk` now advances the contiguous-frame boundary after every chunk. Previously the advancement path was limited to an initial-frame case, which could leave `BufferedThrough` stalled after the first chunk.

2. `NakamaReplayBuffer.Reset` now clears the received replay header as well as frames and end-state data. A reset therefore requires a new replay header before spectator startup can proceed.

3. The Nakama client can transfer ownership of an established `NakamaP2P` UDP socket through `TakeP2PTransport`. The transfer removes the socket from the P2P object's ownership before it is closed.

4. Rollback startup now prefers the retained Nakama UDP socket when the client is a member of a Nakama match and P2P is ready. It verifies that the P2P socket uses the configured `Rollback.Port`, obtains the actual observed remote UDP endpoint, and passes the retained socket to the GGPO transport.

5. `RollbackSession.InitP1` and `InitP2` accept an optional pre-bound UDP socket. Legacy GGPO socket creation remains the fallback when no pre-bound socket is supplied.

6. The spectator loading renderer no longer resets animated text every frame. Text sprites are updated once per frame, and Nakama-specific loading text falls back to the established title loading/wait text when the Nakama text is empty.

7. Strict synchronization validation now rejects a strict-setting schema length/path mismatch before value comparison. This closes the missing-field compatibility hole in the earlier strict comparison.

8. Realtime Lua callbacks are explicitly marshalled onto the engine main-thread task queue instead of invoking gopher-lua from a network goroutine. The queue is best-effort and a saturated queue drops the event rather than blocking the networking goroutine. Callback storage is local to the Lua setup instead of being held in a package-global map.

9. `JoinMatchWithMetadata` supplies the configured game/build identity when the caller does not provide them and no longer mutates the caller's metadata map.

10. Nakama Lua server modules now explicitly import `require("nakama")`, and `nakama/modules/main.lua` registers the authoritative `ikemen_session` and `ikemen_lobby` handlers while loading the matchmaker and rating modules.

11. Lobby join validation now requires game/build identity to match the lobby whenever those lobby identities are populated.

12. The lobby bracket scheduler now consumes automatic byes and advances to the next real match instead of leaving a `player2=nil` pairing active.

13. Nakama `dispatcher.broadcast_message` calls were normalized to the documented four-argument form.

14. `RollbackReplayStream.End` now flushes all remaining authoritative replay frames before publishing the final-frame marker. This closes a short-match/early-end case where the normal ten-second publication window had not elapsed before the fight ended.

15. `currentContentFingerprint()` now produces a deterministic SHA-256 digest from the engine version, active motif/fight-screen definitions, loaded character definitions and character FightFX, the active stage and attached-character definitions, and loaded common FightFX. The digest uses role labels and file contents rather than absolute installation paths.

### External GGPO boundary

The Pass 6 source now contains the IKEMEN-side transport handoff. The remaining dependency-side change is intentionally small: the pinned `github.com/ikemen-engine/ggpo` transport package must expose a constructor that wraps an already-open `net.PacketConn` without binding a second UDP socket. The implementation requirement is documented in `patches/ggpo-nakama-transport.md`.

The dependency is not vendored in the supplied repository and could not be fetched in this environment, so the constructor itself has not been compiled against the exact pinned GGPO source here.

### Ranked-result integrity correction

The earlier Pass 5 description of duplicate settlement protection was stronger than the implementation supports. The current Elo module stores a settlement marker per submitting user. Two reciprocal RPCs can race before either user's marker exists, so the marker is not an atomic match-wide claim. The implementation should therefore be treated as reciprocal-claim reconciliation, not complete duplicate-settlement protection, until settlement is moved into a single authoritative match path or a verified atomic compare-and-swap/transactional storage mechanism is used.

### Validation state

The project `go.mod` is restored to `go 1.27.0`. The available local compiler is Go 1.23.2, and the exact pinned GGPO source is not present in the local module cache. A full engine test was attempted with `GOTOOLCHAIN=local go test ./src` and is blocked immediately because the local toolchain is below the module's declared minimum. Consequently a full engine build has not been claimed.

Focused validation completed successfully:

- standalone P2P ownership tests: `go test ./...` → pass
- standalone replay-buffer/replay-stream tests, including final-frame flushing: `go test ./...` → pass
- Lua syntax checks for all Nakama modules and the example: all pass

### Recovered continuation state

This pass was reconstructed from the supplied Pass 5 source tree plus the later implementation material available in the project. The source tree, rather than an unavailable prior-chat transcript, is the authoritative record for code that is claimed as implemented here.

The next dependency-side blocker remains the pinned GGPO transport adapter. The content fingerprint is now meaningful and hashes the engine version plus active match definitions and loaded FightFX, but it is intentionally not claimed as an exhaustive digest of every dynamically loaded asset or runtime Lua file.


## Appendix B — Pass 7 historical record


## 1. Current architecture

Nakama supplies identity, matchmaking, ratings, lobbies, tournaments, chat,
signaling, and delayed replay distribution. IKEMEN GO remains responsible for
deterministic simulation, GGPO rollback, gameplay transport, replay playback,
Lua scripting, and screenpack presentation.

The Nakama P2P path establishes the UDP socket before rollback starts. The socket
is transferred into GGPO without rebinding so that the NAT traversal state is
preserved.

## 2. Recovered Pass 5/6 implementation

The current tree retains the previously completed work for:

- synchronized configuration schemas and session overrides;
- rollback UDP-port negotiation and compatibility checks;
- replay synchronization headers and restoration;
- content/build fingerprinting;
- Nakama client, matchmaking, lobby, chat and result APIs;
- Nakama Lua server modules and `modules/main.lua` registration;
- Lua-thread-safe event dispatch;
- STUN candidate gathering and UDP hole punching;
- delayed spectator replay buffering and loading presentation;
- replay final-frame flushing;
- Lua bindings for Nakama functionality.

## 3. Pass 7 changes

### 3.1 Bundled GGPO transport adapter

The exact pinned GGPO source is bundled under `third_party/ggpo` and the root
`go.mod` replaces the remote module with that local copy for reproducible builds.

Added:

```go
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp
```

The constructor uses the existing GGPO `Udp` implementation and sender/receiver
lifecycle while adopting an already-bound `net.PacketConn`. It does not create a
second socket. The existing `NewUdp` constructor delegates to the same internal
initializer after binding its own socket.

### 3.2 Nakama P2P self-signal protection

Nakama match broadcasts can be delivered to the publishing client. The P2P
signal handler now ignores a signal whose token is the local token instead of
adopting the local socket as the remote peer.

### 3.3 Focused P2P regression coverage

Added engine-side tests for:

- ignoring self-originated P2P signals;
- completing a two-peer localhost UDP handshake;
- recording the actual observed remote endpoint for each peer.

## 4. GGPO verification

The bundled GGPO transport package passes its lifecycle tests, including the new
`NewUdpFromPacketConn` test covering socket reuse, unchanged local port, packet
receipt, close/unblock, and subsequent port rebind.

The GGPO core packages compile in an isolated validation copy. Existing upstream
GGPO tests unrelated to the transport adapter have pre-existing behavioral
failures in that validation environment; those failures are not represented as
transport-adapter failures.

## 5. Remaining validation boundary

A complete IKEMEN engine build still requires an actual Go 1.27.x compiler and the
project's native/build dependencies. The supplied Go 1.27.1 source archive is not
itself a compiled bootstrap toolchain, and the environment lacks the required
Go 1.24.6-or-newer bootstrap compiler.

The next real acceptance test is an end-to-end two-client match using a real
Nakama server and the bundled GGPO transport:

```text
Nakama authentication
    -> matchmaker
    -> ikemen_session join
    -> P2P signaling
    -> STUN / UDP hole punch
    -> retained UDP socket
    -> GGPO transport
    -> rollback fight
    -> replay publication
```

## 6. Known limitations

TURN/relay fallback is not implemented. Late-joining spectator backfill is not
a complete protocol. Ranked-result handling still uses reciprocal claims and is
not an authoritative deterministic replay verifier. The content fingerprint
covers the configured active match/build definitions and loaded FightFX assets,
but it is not claimed to be an exhaustive digest of every dynamically loaded
runtime asset or Lua module.

The Elo settlement path remains non-atomic: two reciprocal submissions can race
before a match-wide settlement marker exists. It should therefore not be treated
as complete duplicate-settlement protection for a public ranked service.

## 7. Build integration

The development tree contains the GGPO source at `third_party/ggpo` and uses:

```text
replace github.com/ikemen-engine/ggpo => ./third_party/ggpo
```

The standalone dependency patch is also supplied at:

```text
patches/ggpo-nakama-transport.patch
```

so the same change can be applied to the upstream GGPO module instead of using
the bundled source when the dependency is maintained externally.


# Pass 9 — Online Rematch / Session Transition

## Scope

Pass 9 integrates Kamekaze's IKEMEN GO rematch module with the existing Nakama coordination session and GGPO/P2P lifecycle. The goal is to make the post-match decision deterministic across both peers without replacing GGPO or creating a new Nakama coordination match for every rematch.

## 1. Online decision protocol

The existing Nakama `ikemen_session` coordination match now recognizes two opcodes:

- `OP_REMATCH_CHOICE = 1229673027`
- `OP_REMATCH_DECISION = 1229673028`

Each client sends `{version, action, round, snapshot}` using `nakama.rematchChoice()`. Valid actions are `rematch`, `select`, and `exit`. The Nakama match broadcasts one authoritative decision to both players.

Decision semantics:

- Either `exit` -> both peers exit.
- Otherwise either `select` -> both peers return to character select.
- Only when both choose `rematch` -> both peers start the next game without VS/order selection.

The coordination match stays alive across these transitions. This is a new gameplay/rollback session, not a second persistent Nakama matchmaking session.

## 2. Versus snapshot

`start.f_captureOnlineRematchSnapshot()` records the resolved post-VS state:

- stage number;
- P1/P2 team mode;
- ordered character references;
- selected palettes.

The capture runs after the online versus screen, including order selection. Numeric character references are accepted by `launchFight()` so the rematch does not depend on character-name lookup or local ordering.

The server validates snapshot structure, positive integer stage/character/palette values, and team-mode fields. When both players choose rematch their snapshots must agree; a mismatch becomes a Character Select decision instead of allowing one peer to impose a different game state.

## 3. GGPO/P2P lifecycle

A major lifecycle constraint is that the current GGPO backend owns the active UDP socket after the Nakama P2P socket has been transferred. The next P2P socket therefore cannot be created while the current `game()` call is still active.

Pass 9 moves P2P startup to the mode loop after the current gameplay session has returned through `start.f_game()`. This guarantees that rollback teardown has already closed the previous GGPO socket.

Character Select starts a fresh P2P socket before entering selection and waits for it to become ready after selection finishes. A rematch starts a fresh P2P socket after the previous match and waits for readiness before launching the forced character/stage fight.

## 4. Rematch launch

Rematches bypass VS and order select with:

```text
onlineSession = true
vsscreen = false
p1orderselect = false
p2orderselect = false
continue = false
quickcontinue = true
stageNo = saved stage
forceStage = true
p1char / p2char = saved teams
```

`start.f_setStage()` gained a distinct `forceStage` argument so `main.stageMenu` cannot replace the negotiated stage. This is intentionally separate from ordinary `assigned` stage behavior and does not change continued-match stage selection.

## 5. Ranked side-switch interaction

Ranked side switching is applied by `start.f_game()` after the current `game()` call records the winner. A VS snapshot is physically side-oriented, so `start.f_prepareOnlineRematchSnapshot()` swaps the two sides of the snapshot after a non-final ranked match when `switch_sides` is enabled. This keeps each stable set player associated with the side assigned by `RankedSet`.

Character Select already uses the ranked carry-selection mechanism, so the same side assignment applies to the select path.

## 6. Ranked final-match behavior (Pass 9 historical state)

Pass 9 originally suppressed the post-final-match rematch menu so a completed set returned directly to the online menu. This behavior is superseded by Pass 10.

## 7. Online UI state (Pass 9 historical state)

Pass 9 used a three-choice online UI: Rematch, Character Select, Exit. Pass 10 extends that UI based on ranked-set state while keeping the legacy offline Kamekaze flow unchanged.

The module temporarily disables the local Kamekaze Yes/No override only while the Nakama online rematch flow is active and restores the previous override value when online mode ends.

## 8. Exit path

Exit does not leave Nakama from inside the live gameplay callback. The active gameplay session is first terminated, allowing normal post-match bookkeeping to complete. `start.f_selectMode()` then stops/leaves the Nakama coordination session and returns from `netplayversus` to its existing caller/menu.

## 9. Correctness fixes retained from Pass 9

- removed premature P2P restart from the rematch callback;
- removed premature Nakama match leave from the exit callback;
- preserved user state for `ik_rematch.override`;
- forced the negotiated rematch stage despite `main.stageMenu`;
- preserved ranked player identity across side-switching rematches;
- rejected malformed and mismatched rematch snapshots server-side.

## 10. Pass 10 — Ranked set post-match decisions

### 10.1 Non-final ranked match

After every ranked match that does not complete the negotiated best-of-X set, the online rematch menu presents exactly three actions:

```text
Rematch          both players must choose -> start a new gameplay session using the captured VS snapshot
Character Select  either player chooses -> start a fresh selection session
Forfeit           the choosing player concedes the ranked set -> both return to the online mode menu
```

The third entry is deliberately labeled `Forfeit` instead of `Exit` so that leaving before the set is complete is represented explicitly as a set loss.

### 10.2 Final match of the set

When the current match will give one player the majority required by `BestOf`, the prompt changes to exactly two actions:

```text
Play Another Set  both players must choose -> reset the completed set and return to Character Select
Exit              either player chooses -> both return to the online mode menu
```

The final prompt is shown after the final match rather than being suppressed. `play another set` does not reuse the completed set's score or winner-selection lock. It starts a new set with the same two matched player identities and negotiated rules, resets the score to 0-0, and restores the default side assignment before Character Select.

### 10.3 Deterministic decision flow

`ik_rematch.lua` determines whether the current match is the final match from the current physical winner side and the ranked-set state before `RankedSet.RecordMatch()` records the completed game. It transmits a `set_final` flag with the decision request.

The Nakama coordination match treats `set_final` as a round-level property once any valid final-state request has been observed. It accepts `forfeit` only for an in-progress ranked set and accepts `new_set` only for a completed ranked set. Invalid context/actions resolve safely instead of leaving the coordination round unresolved.

### 10.4 Forfeit application

A server-issued `forfeit` decision includes the source Nakama user ID. The client maps that stable user identity to the active `RankedSet` player identity and calls `nakama.rankedSetForfeit()`. The forfeiting player becomes the set loser without fabricating an additional normal match win. The set is then ended through the existing online-menu transition.

A player leaving while a non-final ranked decision is outstanding is also treated as a forfeit by the coordination match, using the actual leaving user's ID rather than the remaining user's ID. A leave during the final-set prompt is resolved as `exit`, because the completed set no longer has an unfinished-set forfeit consequence.

### 10.5 RankedSet lifecycle changes

`RankedSet.Forfeit(player)` ends the active set in favor of the other stable player without changing `matches` or either normal match-win counter. `RankedSet.StartNextSet()` now requires a completed set and resets:

- set winner;
- last match winner;
- match count;
- player wins;
- physical side assignment;
- winner-selection tokens.

The same stable players and negotiated set rules remain active.

### 10.6 Game-loop sequencing

The post-game launch loop does not force an immediate exit when `rankedSetFinished` is true if `start.onlineNextAction` already contains `new_set`, `exit`, or `forfeit`. This allows the completed game to return to `start.f_selectMode()`, where the coordinated next-session action is consumed.

`new_set` starts a fresh Nakama P2P/GGPO session and enters Character Select. It does not reuse the prior VS snapshot. A normal `rematch` still reuses the captured snapshot and continues to apply ranked side-switching to that snapshot when configured.

## 11. Pass 10 implementation files

```text
external/mods/ik_rematch.lua
external/mods/rematch.def
external/script/start.lua
nakama/modules/ikemen_session.lua
nakama/README.md
src/nakama_script.go
src/ranked_set.go
src/ranked_set_test.go
IKEMEN-GO-Nakama-Implementation-pass10.md
```

## 12. Validation status

The standalone ranked-set tests pass with the available Go compiler when run outside the module's Go 1.27 requirement. The Lua source for `external/mods/ik_rematch.lua`, `external/script/start.lua`, and `nakama/modules/ikemen_session.lua` parses successfully with the local LuaTeX Lua interpreter.

The full IKEMEN build remains unverified in this environment because the installed compiler is Go 1.23.2 while the project declares Go 1.27.0. The supplied Go 1.27.1 archive is source code and requires a newer bootstrap Go toolchain.

The source-level ranked rematch flow is therefore implemented and internally tested, but a real two-client Nakama session still needs to be exercised on a machine with the complete Go 1.27.x and target-platform dependency toolchain.

## 13. Production-ranked limitations

The Pass 10 change does not claim authoritative persistent rating/result settlement or deterministic replay verification. It defines the client/session behavior for forfeits and additional sets. Existing Pass 9 limitations around ranked-result authority, replay verification, and TURN fallback remain unchanged.

## 14. Record status

This Pass 10 record supersedes the Pass 9 final-match suppression behavior and the Pass 9 three-choice ranked prompt description. Earlier Pass 6–9 work remains preserved in this consolidated record and in the corresponding implementation files. The source tree is authoritative for the implemented state.


---

# Pass 11

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass11.md`


# Pass 11 — Lobby queue rotation and winner-stays game cap

## 11.1 Default lobby format

The authoritative Nakama lobby now defaults to `queue` rather than `winner_stays_on`.

The queue is FIFO:

```text
waiting line: A -> B -> C -> D -> E
active turn:  A vs B
```

When the active match ends, both participants are appended to the end of the waiting line and the next two waiting players take the active turn:

```text
before:  A vs B | C D E
result:  A/B return to the end
next:    C vs D | E A B
```

The active game pair is removed from the waiting queue while playing. This makes the queue explicit and prevents the active players from occupying their own waiting positions.

## 11.2 Active queue membership

Players may join an active `queue` or `winner_stays_on` lobby until the configured eight-player capacity is reached. A newly joined player is appended to the end of the waiting queue rather than interrupting the active match.

A departure that invalidates an active pairing resets and immediately rebuilds the queue schedule from the remaining members so an active lobby can resume without requiring another player to join.

## 11.3 Winner-stays-on game cap

`winner_stays_on` remains available as an alternate lobby format.

Its optional `max_games` value limits how many consecutive completed matches the current winner may play before returning to the end of the queue. The normalized range is 1 through 99; the default is 3.

Example with four players and `max_games = 3`:

```text
1. A vs B -> A wins
2. A vs C -> A wins
3. A vs D -> A wins
4. B vs C -> next turn after A reaches the cap
```

The loser always goes to the end of the waiting queue. When the winner reaches the cap, the winner is also appended to the end and the next two queued players take the match. With only two players, the same two participants can naturally meet again because there is no third waiting player.

A `max_games` value supplied under `rules.max_games` is honored for compatibility with generic lobby rules. A top-level `max_games` value is also supported by the client/server API.

## 11.4 Lua/client API

`nakama.createLobby()` now defaults to:

```lua
nakama.createLobby({
    name = "Open Lobby",
    format = "queue",
    max_players = 8
})
```

Winner-stays-on example:

```lua
nakama.createLobby({
    name = "Winner Stays",
    format = "winner_stays_on",
    max_players = 8,
    max_games = 3
})
```

`NakamaLobbyConfig` exposes `MaxGames`, while `Rules["max_games"]` remains accepted for generic rule payloads.

Lobby state/pairing broadcasts now expose `format`, `max_games`, the current waiting `queue`, and the current winner streak so screenpacks can present the scheduling state without adding a second hardcoded renderer.

## 11.5 Existing tournament formats

`round_robin` and `bracket` remain available and keep their existing fixed tournament scheduling semantics. The new FIFO queue behavior is specific to the default `queue` format and the optional `winner_stays_on` format.

## 11.6 Validation

The following source-level checks were performed for Pass 11:

- Go formatting completed for modified Go files.
- Lobby-format and `max_games` normalization logic is covered by new Go unit tests in `src/nakama_lobby_test.go`.
- FIFO and winner-stays scheduling behavior was exercised with an independent deterministic state-machine check using the same transition rules implemented by `ikemen_lobby.lua`.
- The Nakama README and example now document both queue and winner-stays lobby modes.

The environment still lacks an executable Go 1.27.x toolchain and a Lua/Nakama runtime, so the full engine build and live Nakama server test remain deployment-host validation tasks.

## 11.7 Current status

Pass 11 supersedes the previous default lobby scheduling behavior. The previous online rematch/ranked-set implementation is retained unchanged. Lobby scheduling remains outside GGPO rollback state and is controlled by the Nakama coordination match.


---

# Pass 12

> Source record: `/mnt/data/IKEMEN-GO-Nakama-pass12/IKEMEN-GO-Nakama-Implementation-pass12.md`

# IKEMEN GO Nakama Implementation — Pass 12

## Scope

Pass 12 adds an optional IKEMEN-side server launcher/manager and shipped Nakama client server profiles. The feature is designed so a distributed game can contain a prewritten JSON profile for its intended online service while dedicated-community hosting remains optional.

## Client connection model

The client profile is `data/online/servers.json`.

A game can call:

```lua
nakama.connect()
```

to use the profile marked as `default`.

A partial configuration can override the shipped profile:

```lua
nakama.connect({
    game = "my_game",
    build = "1.0.0"
})
```

A specific profile can be selected with `server_id`:

```lua
nakama.connect({server_id = "community-west"})
```

The selected profile supplies host, port, server key, TLS/status settings, game/build/region metadata, and the default STUN server list.

Command-line selection is also supported:

```text
-nakama-server <id>
-nakama-config <path>
```

This makes the server endpoint a distribution-level setting rather than a value hardcoded into the game script.

## Optional dedicated server operation

Dedicated hosting is not required for ordinary online play. A distributed client connects to the Nakama endpoint in its shipped profile and uses the existing Nakama/GGPO architecture.

A community host can instead run:

```text
Ikemen_GO -server-wizard
Ikemen_GO -server -server-config save/server.json
```

The wizard is terminal-only/headless. It does not initialize the game's SDL renderer.

The wizard collects:

- server name and game identifier
- Nakama executable/configuration paths
- bind/public address
- Nakama HTTP/socket, gRPC, and console ports
- server key and TLS client setting
- matchmaking/lobby/ranked/spectator/chat enablement
- ranked best-of, side switching, and winner-selection rules
- default lobby format and winner-stays game cap
- database DSN metadata and server log path

It writes an IKEMEN server-manager profile and a companion client connection profile. The client profile can be copied into the game's `data/online/servers.json` after the public endpoint has been verified.

The server manager launches an external Nakama executable. IKEMEN does not embed the Nakama server runtime or database. The dedicated host therefore still supplies Nakama's own configuration and database deployment.

## Existing work carried forward

Pass 12 preserves all work from Pass 6 through Pass 11, including:

- synchronization schema and session-wide host/strict configuration handling
- replay headers and replay validation plumbing
- rollback replay stream and delayed spectator buffering
- STUN candidate gathering and UDP hole punching
- retained NAT-traversed UDP socket ownership transfer into GGPO
- bundled GGPO transport constructor for an already-open `net.PacketConn`
- Nakama authentication, realtime events, matchmaking, lobbies, chat, Elo/result coordination
- ranked Best-of-X set handling
- side switching between matches
- winner-keeps-character/team rule
- rematch, character-select, forfeit, and play-another-set flows
- default FIFO lobby queue
- optional winner-stays lobby with a configurable 1–99 game cap
- independent 16:9/16:10 border presentation canvas and 4:3 game viewport
- border Lua/UI hooks
- content fingerprinting and synchronized build identity plumbing

## Implementation files added/changed in Pass 12

- `src/nakama_profile.go` — shipped client server profile loader/selector
- `src/nakama_profile_test.go` — profile selection/loading tests
- `src/server_manager.go` — dedicated server profile, headless wizard, client-profile export, and Nakama process launcher
- `src/server_manager_test.go` — normalization/wizard tests
- `src/nakama_client.go` — profile-provided default STUN servers
- `src/nakama_script.go` — shipped-profile-aware `nakama.connect()` and `server_id` selection
- `src/main.go` — `-server`, `-server-wizard`, `-server-config`, `-nakama-server`, and `-nakama-config` command-line handling
- `data/online/servers.json` — distributed-client profile template
- `data/online/README.md` — client profile documentation
- `nakama/server.example.json` — dedicated server-manager profile example
- `nakama/README.md` — server profile and launcher documentation

## Validation

Passed in an isolated Go 1.23 compatibility harness for the Pass 12-only components:

- Nakama server profile default selection
- Nakama server profile JSON loading
- server configuration normalization
- server wizard JSON creation
- companion client profile generation

The full IKEMEN package build remains a Go 1.27.x build-host task because the current environment only has Go 1.23.2 and cannot fetch the Go 1.27 toolchain due to network restrictions.

The server launcher has not been validated against a live Nakama executable in this environment. End-to-end deployment still requires a real Nakama binary/configuration/database and an online client test.

## Current architectural boundary

```text
Distributed game
    |
    +-- data/online/servers.json
    |       |
    |       +-- remote Nakama deployment (normal case)
    |
    +-- optional dedicated host
            |
            +-- Ikemen_GO -server-wizard
            |
            +-- save/server.json
            |
            +-- Ikemen_GO -server
                    |
                    +-- Nakama executable
                            |
                            +-- Nakama modules
                            +-- database

Client gameplay remains:

Nakama control plane -> P2P/STUN -> existing UDP socket -> GGPO -> deterministic IKEMEN simulation
```
