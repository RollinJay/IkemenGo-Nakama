# IKEMEN GO Nakama Matchmaking Layer

This integration adds an engine-side Nakama client, Lua bindings, ranked/unranked matchmaking, authoritative 8-player lobby coordination, chat access, Elo result reconciliation, NAT-discovery/hole-punching primitives, and rollback-resolved replay streaming.

## Runtime model

Nakama is the control plane. IKEMEN GGPO remains the gameplay simulation and rollback system.

```text
Nakama
  ├─ authentication
  ├─ ranked / unranked matchmaking
  ├─ Elo storage and result reconciliation
  ├─ authoritative lobbies (up to 8 players)
  ├─ lobby discovery
  ├─ chat
  └─ P2P signaling / delayed replay distribution

IKEMEN / GGPO
  ├─ deterministic simulation
  ├─ rollback
  ├─ gameplay UDP transport
  └─ replay input recording
```

The engine's existing TCP-derived UDP setup in `src/rollback.go` is now conditionally replaced when a Nakama P2P connection is established. The established UDP socket is transferred into the GGPO transport so the NAT mapping is preserved; the remote endpoint is the endpoint observed by the successful P2P handshake rather than the TCP-negotiated rollback port.

The pinned `github.com/ikemen-engine/ggpo` dependency is bundled under `third_party/ggpo` with the small `transport.NewUdpFromPacketConn(messageHandler, net.PacketConn)` constructor required by the Nakama transport handoff. The reproducible patch is retained under `patches/ggpo-nakama-transport.md`.

## Replay streaming

`RollbackReplayStream` publishes only resolved input frames that are older than the configured broadcast delay. The default is 10 seconds:

```text
60 frames/second × 10 seconds = 600 frames
```

Replay chunks are 30 frames. The existing IKEMEN replay input representation is reused: 8 controller slots × 8 bytes per controller per frame.

A rollback that reaches an already-published frame invalidates the current stream and sends a reset message instead of allowing a speculative frame to remain visible to spectators.

`NakamaReplayBuffer` retains the received deterministic input history. Lobby members that were present from the start of a match can therefore reconstruct the delayed timeline locally without receiving rendered video or live game state.

## Lua API

The global `nakama` table exposes:

```lua
nakama.connect({
    host = "127.0.0.1",
    port = 7350,
    server_key = "defaultkey",
    ssl = false,
    game = "my_game",
    build = "1.0.0",
    region = "us-west"
})

nakama.on("connected", function(event, data) end)
nakama.on("matchmaker_matched", function(event, data) end)
nakama.on("match_joined", function(event, data) end)
nakama.on("chat_message", function(event, data) end)
nakama.on("replay_chunk", function(event, data) end)
nakama.on("p2p_connected", function(event, data) end)

nakama.matchmake("ranked", 1000, 100)
nakama.matchmake("unranked")

nakama.createLobby({
    name = "Friday Night Fights",
    -- Default format is FIFO queue: after playing, both players go to the end.
    format = "queue",
    max_players = 8,

    -- Used by winner_stays_on only: cap the winner's consecutive games.
    max_games = 3,
    rules = {
        rounds = 3
    }
})

nakama.listLobbies()
nakama.joinMatch(lobby_id)
nakama.joinChat(lobby_id)
nakama.sendChat(channel_id, "hello")
nakama.reportLobbyResult(winner_user_id, loser_user_id)
nakama.submitResult(match_id, opponent_user_id, "my_game", 1, replay_hash)

-- The engine's live replay buffer is ready once 600 frames have arrived by default.
-- This starts native replay playback after game-specific match setup is ready.
nakama.startSpectatorReplay()
```

## Screenpack integration

All engine-rendered Nakama UI is motif-backed. The default motif defines a dedicated `[Nakama Info]` section and optional `[NakamaBgDef]`. Existing screenpacks remain compatible because the spectator loading screen falls back to `[Title Info]` `loading.wait` when the Nakama-specific loading text is unavailable.

The screenpack can style the replay loading screen through:

```ini
[Nakama Info]
title.text = NAKAMA
loading.text = BUFFERING REPLAY...
loading.progress.text = %d%%
loading.detail.text = %d SECONDS REMAINING

[NakamaBgDef]
; normal bgdef keys
```

`motifState("nakama")` reports whether the Nakama client is connected. `motifState("nakama_spectator_loading")` reports whether the engine is currently displaying the delayed spectator loading screen. `motifVar("nakama_info.*")` can be used by Lua-driven screenpacks to read or customize the motif values. The default status map includes `connected`, `matchmaking`, `matched`, `connecting`, `lobby`, and `spectating` entries.

Lobby browsers, tournament displays, player lists, and chat are intentionally exposed through the Nakama Lua data/events rather than introducing a second hardcoded menu renderer. A screenpack can therefore build those screens with the same Lua/UI facilities used elsewhere in IKEMEN. The engine does not impose a second hardcoded lobby renderer.

### Online rematch coordination

The Kamekaze `ik_rematch.lua` module is extended for Nakama netplay. The post-match choices depend on ranked-set state:

**Unranked:**

```text
Rematch          both players must choose it -> new rollback gameplay session
Character Select  either player chooses it -> both return to character select
Exit              either player chooses it -> both leave the online session
```

**Ranked, set still in progress:**

```text
Rematch          both players must choose it -> new rollback gameplay session using the resolved VS snapshot
Character Select  either player chooses it -> both return to character select
Forfeit           the choosing player concedes the ranked set -> both return to the online menu
```

**Ranked, final match completed:**

```text
Play Another Set  both players must choose it -> reset the current set and return to character select
Exit              either player chooses it -> both leave the online session
```

The set result is tracked separately from GGPO rollback state. Starting another set resets the score and physical side assignment while retaining the same two matched players and negotiated ranked rules. A new set does not carry forward winner-selection locks from the previous set.

The Nakama coordination match is retained between rounds; only the GGPO gameplay session is replaced. The old GGPO UDP socket is closed before the next Nakama P2P socket is created.

The previous versus-screen snapshot contains the resolved stage, team mode, character references, and palettes. A rematch reuses that snapshot and bypasses VS/order select. Ranked side-switching is applied after the completed match, so a rematch snapshot is transformed to keep each stable ranked-set player attached to their newly assigned physical side.

The server validates both rematch snapshots and requires them to agree before issuing the rematch decision. A malformed or mismatched snapshot falls back to Character Select.

The online menu returns through the existing `netplayversus` caller path; no separate hardcoded online-mode renderer is required.

### Lobby queue scheduling

`queue` is the default lobby format. Players enter a FIFO waiting line. The first two players take the active turn; when their match ends, both players are appended to the end of the line and the next two waiting players are paired. New players may join an active `queue` or `winner_stays_on` lobby until it reaches eight members.

`winner_stays_on` is an optional alternative. The winner remains active against the next queued player until `max_games` consecutive matches have been played by that winner. The completed winner and loser are then both returned to the end of the queue. `max_games` is normalized to an integer from 1 through 99 and defaults to 3. If the lobby has fewer than three players, the same participants may naturally meet again because there is no third waiting player.

Existing `round_robin` and `bracket` formats remain available and keep their fixed tournament scheduling semantics.

### Lobby scheduling

`queue` is the default lobby format. Players are maintained in FIFO order: the first two waiting players take the match, and after the match both return to the end of the line. New players join the end of the queue.

`winner_stays_on` is an optional format. The winner remains active against the next queued player, but only for `max_games` consecutive matches before the winner returns to the end of the queue. `max_games` defaults to 3 and is clamped to 1-99. It may be supplied as a top-level `max_games` value or as `rules.max_games`.

The lobby state exposes the waiting queue, current format, maximum winner streak, and current winner streak to Nakama-aware screenpacks.

### P2P primitive

The current Lua surface exposes:

```lua
nakama.startP2P(7500, "stun.example.net:3478")
nakama.stopP2P()
```

When the first argument is omitted or zero, the engine uses `Netplay.Rollback.Port`. The P2P socket must be created on that same port because the engine transfers the already-bound socket into GGPO rather than rebinding it.

This starts IPv4 host/server-reflexive candidate gathering and UDP hole punching. The UDP socket is retained after the handshake so the eventual GGPO transport can take ownership of it. It is not the final GGPO transport by itself.

Start `nakama.startP2P()` before rollback creates the GGPO peer. The current engine then transfers the established Nakama P2P UDP socket into GGPO instead of binding a second socket, using the punched peer endpoint as the rollback destination. The bundled GGPO dependency already contains the transport constructor described in `patches/ggpo-nakama-transport.md`.

## Server modules

The runtime entrypoint is `nakama/modules/main.lua`. Install all five files into Nakama's runtime module path:

- `main.lua` — loads and registers the IKEMEN match handlers and supporting modules.
- `ikemen_matchmaker.lua` — pairs ranked/unranked tickets and creates an `ikemen_session` match.
- `ikemen_session.lua` — Nakama coordination match for matched players, signaling, and replay chunks; it does not simulate the fight.
- `ikemen_lobby.lua` — authoritative 8-player lobby coordination.
- `ikemen_rating.lua` — per-game Elo storage and reciprocal result settlement.

The individual Lua modules explicitly import Nakama's `nakama` runtime module; they do not rely on an implicit global.

The matchmaker reads Elo from server-side storage. The client-supplied `elo` field is metadata only and is not used as the trusted rating source.

The result settlement code requires reciprocal claims before updating ratings. It is not an anti-cheat system. The current storage-based settlement marker is not an atomic distributed transaction, so production ranked deployment still requires either single-match authoritative settlement or a verified compare-and-swap/transactional claim before treating duplicate settlement prevention as complete.

## Game compatibility

The synchronized replay header carries IKEMEN's sync settings and a SHA-256 content fingerprint. The current fingerprint covers the engine version, active motif/fight-screen definitions, loaded character definitions and character FightFX, the active stage and attached-character definitions, and loaded common FightFX. It intentionally hashes file contents rather than absolute filesystem paths.

The fingerprint is now meaningful, but it is not an exhaustive record of every possible dynamically loaded asset or runtime Lua file. Ranked deployment should treat it as one compatibility layer alongside Nakama game/build identity and should not consider it a substitute for authoritative replay verification.

## Current implementation boundary

Implemented in this tree:

- Nakama REST device authentication.
- Nakama realtime WebSocket connection and event dispatch.
- Ranked/unranked matchmaker tickets.
- Automatic join of the Nakama coordination match after matchmaking.
- Server-side Elo storage and reciprocal result reconciliation.
- Authoritative lobby creation/listing and up-to-8 lobby state.
- Mandatory lobby game/build metadata compatibility checks.
- Nakama chat.
- Ten-second rollback replay publication with rollback invalidation.
- Client-side replay chunk buffering, including out-of-order chunk acceptance with contiguous-frame advancement.
- STUN Binding Request parsing, host/server-reflexive candidate gathering, and UDP hole punching.
- Retained NAT-traversed UDP socket ownership transfer.
- GGPO startup path that can consume the retained Nakama socket through the pinned dependency extension.
- Lua/main-thread event marshalling.
- Explicit Nakama Lua server module entrypoint and match-handler registration.
- Motif-backed spectator loading UI with a fallback to the existing title loading motif.

Still requiring completion or validation:

- Validate the full engine build and end-to-end Nakama/P2P/GGPO match on a Go 1.27.x build host.
- TURN/relay fallback for networks where direct UDP traversal fails.
- Fully generic live spectator initialization from arbitrary game/lobby metadata.
- Historical replay backfill for spectators who join after publication has already advanced.
- Atomic/authoritative ranked-result settlement and deterministic replay verification.
- Meaningful `currentContentFingerprint()` coverage for production compatibility gating.
- Production Nakama deployment configuration and real two-peer NAT/rollback testing.

## Shipped client server profiles

Distributed game builds can ship `data/online/servers.json` with the game's intended Nakama deployments. Calling `nakama.connect()` with no arguments starts from the profile marked by `default`; a game can override individual fields or select another profile with `server_id`. STUN servers listed in the selected profile become the default P2P STUN list.

Example:

```json
{
  "version": 1,
  "default": "official",
  "servers": [
    {
      "id": "official",
      "name": "Official Online Server",
      "host": "online.example.com",
      "port": 7350,
      "server_key": "defaultkey",
      "ssl": true,
      "status": true,
      "game": "my_game",
      "build": "1.0.0",
      "region": "us-west",
      "stun_servers": []
    }
  ]
}
```

This file is a client connection profile, not a secret. Replace it per distributed game build so ordinary players do not need to configure their Nakama endpoint manually.

## Optional dedicated-server launcher

Dedicated servers are optional. A normal player does not need a local Nakama process when the game ships with a remote profile. A community that wants to operate its own deployment can use IKEMEN's headless command-line server tools:

```text
Ikemen_GO -server-wizard
Ikemen_GO -server -server-config save/server.json
```

`-server-wizard` is intentionally headless: it prompts in the terminal and writes an IKEMEN server-manager JSON profile. The wizard also writes a companion `save/server-client.json` profile that can be copied into `data/online/servers.json` after the public hostname, key, and TLS settings are verified.

`-server` launches the configured Nakama executable as a child process, forwards its stdout/stderr, and can append to a configured log file. IKEMEN is the launcher/manager; it does not embed the Nakama server or its database. The community server still supplies the Nakama binary, Nakama runtime configuration, and required database deployment.

The dedicated server configuration includes the intended matchmaking, lobby, ranked, spectator, chat, queue/winner-stays defaults and port assignments so the community deployment is documented in one place.
