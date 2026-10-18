# IKEMEN GO Nakama Matchmaking Layer

This integration adds an engine-side Nakama client, Lua bindings, ranked/unranked matchmaking, authoritative 8-player lobbies with screenpack-configurable lobby screens, online names chosen in the game and shown on the fight screen, chat access, Elo ratings changed once per ranked set when both players' claims of its winner agree (with both players' replays of the set kept on the server for its moderators) and shown as levels that each server sets, with a leaderboard per game, NAT-discovery/hole-punching primitives, rollback-resolved replay streaming, which lets lobby members watch the match being played, and match replays, which save online matches (and, optionally, local ones) with their description and the players' accounts.

## Runtime model

Nakama is the control plane. IKEMEN GGPO remains the gameplay simulation and rollback system.

```text
Nakama
  ├─ authentication and online names
  ├─ ranked / unranked matchmaking
  ├─ Elo storage, ranked sets and their replays (for moderators)
  ├─ levels, leaderboards and ranked notifications
  ├─ authoritative lobbies (up to 8 players)
  ├─ lobby discovery and room IDs
  ├─ chat
  └─ P2P signaling / delayed replay distribution

IKEMEN / GGPO
  ├─ deterministic simulation
  ├─ rollback
  ├─ gameplay UDP transport
  └─ replay input recording
```

Nakama only passes connection details. Both players' clients exchange UDP candidates through the Nakama match, punch a path through their NATs, and then keep that one UDP socket for the whole online session (see [Online sessions](#online-sessions)). IKEMEN's netplay session (config sync, seed, menu lockstep, loading rendezvous) runs over it as a reliable KCP stream, and each rollback match gets its own GGPO channel on the same socket. When a Nakama P2P connection is ready, it replaces the TCP connection and the TCP-negotiated rollback UDP port.

The pinned `github.com/ikemen-engine/ggpo` dependency is bundled under `third_party/ggpo` with the small `transport.NewUdpFromPacketConn(messageHandler, net.PacketConn)` constructor required by the Nakama transport handoff. The reproducible patch is retained under `patches/ggpo-nakama-transport.md`.

## Replay streaming

A rollback match played inside a Nakama match (a lobby, or the coordination match of a matchmaker pairing) is published as a replay stream: both players' inputs, taken from the rollback session once they can no longer change, sent through the Nakama match. A client with the same game content replays the match from those inputs; no video and no game state is sent. The lobby screens use this to let members watch the match being played (see [Watching a lobby match](#watching-a-lobby-match)).

The netplay session's host publishes the stream: in a lobby, the pairing's player 1; after matchmaking, the first player of the matchmaker's result. Publishing is on by default; `nakama.setReplayPublish` turns it off, or changes the delay and the match description, for the matches that follow (see [Lua API](#lua-api)). A ranked session is the exception: both players publish their stream, whatever `nakama.setReplayPublish` said, with a delay of 2 seconds (enough to wait out rollbacks), and the server keeps both streams for its moderators instead of relaying them (see [Ranked sets](#ranked-sets)). Their header and end carry more than other streams' do (see the table below), so that the server can make match replay files of them.

Only frames older than the broadcast delay are sent. The delay is `Netplay.Rollback.ReplayBroadcastDelay` (10 seconds by default) or the delay given to `nakama.setReplayPublish`:

```text
60 frames/second × 10 seconds = 600 frames
```

A Turns match is played as one rollback session per character change, and each session publishes a stream of its own. The streams of one match share the match description and are numbered by their segment: 0 for the stream that starts in the match's first round, then 1, 2 and so on.

A stream consists of four kinds of message:

| Message | Content |
|---|---|
| Header | A stream ID; the delay; IKEMEN's sync settings and content fingerprint (the host's settings that the netplay session applied to both players); the random seed and the frame counters at the start of the match; the stage (its definition file, after a random choice is resolved); the rules the match started with (round time, ticks per timer count, rounds each side needs to win, draws allowed); the match context (match number, home side, each side's consecutive wins and score before the match, and the rounds counted so far in a run whose rounds carry over); the game parameters (whether life, rounds and music carry over from the previous match, the Turns member each side starts with, whether characters' dialogues play); the input slot each player reads and each player's AI level; the publisher's match description; the segment number; the version of the game that published it; and, in a ranked session, each player's state at the start (as in a match replay file). |
| Chunk | 30 frames of input in IKEMEN's replay input representation: 8 controller slots × 8 bytes per controller per frame. A chunk is sent once all of its frames are older than the delay, so a stream sends two chunks per second; the last chunk of a stream can be shorter. |
| End | The last frame of the stream; in a ranked session, also the files the match depends on, with the start of their SHA-256, as a match replay file lists them. |
| Reset | The stream is withdrawn, with a reason. |

A rollback that reaches an already-published frame withdraws the stream with a reset message instead of allowing a speculative frame to remain visible to spectators.

Each client keeps the 16 most recent streams it received, one `NakamaReplayBuffer` per stream ID. A buffer stores its stream's frames in order, accepts chunks that arrive out of order or twice, and ignores data of other streams. `nakama.startSpectatorReplay()` plays a received match as a replay (a live replay):

- It starts from segment 0 of the match that the latest stream belongs to, and continues with the next segment each time a Turns match changes characters.
- It starts once 60 frames are buffered (or the stream's delay, when that is shorter), or once the stream has ended.
- It refuses to start when this game's content fingerprint differs from the one in the header (see [Game compatibility](#game-compatibility)).
- The header's rules (round time, ticks per timer count, rounds to win, draws), match context and game parameters replace the ones the spectator's screens set up, so the spectator's fight ends when the players' did, and each player reads the input slot it read in the players' games.
- Frames that have not arrived are waited for, up to 20 seconds plus the stream's delay, with the window kept responsive; a stream that is withdrawn, or stalls for longer, ends the replay.
- The Pause key pauses a live replay without losing its place.
- A spectator more than 240 frames (4 seconds) behind the newest frame received plays at 4× speed until 90 frames are left, so that a spectator who starts late finishes close to the players. The end of a stream sends its last seconds at once; those count as not yet received, so the end of a match plays at normal speed.

## Match replays

A match replay is a file that holds one match: a description of the fight, the files its simulation depends on, and its inputs. WATCH MODE > REPLAY plays it by loading the fight it describes, as a lobby spectator loads a watched match (see [Watching a lobby match](#watching-a-lobby-match)), instead of replaying the menus that led to the fight. A roster or screenpack changed since the recording therefore does not change the characters or the stage that load.

### Which matches are saved

The game saves match replays to `save/replays`:

- **Online matches**, with rollback netcode: every match of a netplay session, versus or co-op (two players on side 1 against the CPU). Both players' games save it; lobby matches, matchmade matches and direct Host/Join matches are saved alike. Online play through Nakama always uses rollback netcode (see [Netcode](#netcode)); Host/Join matches use it when the Netcode option is Rollback.
- **Local versus matches**, when the Local Versus Replays option is on (`Config.LocalVersusReplays`, Options > Engine Settings; off by default): matches with players on both sides, in VS Mode, Team Versus, Versus Co-op, and a challenger's match in arcade (the modes in `replay.versusModes`).
- **Matches against the CPU**, only in the game modes a game lists in `replay.recordModes` (none by default), for example the modes of its score or challenge leaderboards. Training is never saved: its settings change the match outside its inputs.

Each file is named after the match's date and its two sides: the players' online names and tags for players who played online, otherwise the name of the side's first character. For example `2026-09-29_17h25m03s KAI (7841) vs ElPeleador (6321).replay`, `2026-09-30_00h11m50s KAI (7841) & ElPeleador (6321) vs Kung Fu Man.replay` for a co-op match, and `2026-09-30_00h48m50s Kyo vs Iori.replay` for a local match.

A game whose leaderboards want a replay with each score can send the file of the match that set it: `replayLastMatchFile()` returns the last file saved. The server does not check replays: it keeps the replays of ranked sets for its moderators to watch (see [Ranked sets](#ranked-sets) and [Current implementation boundary](#current-implementation-boundary)).

### What a file holds

| Part | Content |
|---|---|
| Match | The engine version and the date. The files the match's simulation depends on, with the start of their SHA-256: each character's definition, command, state and animation files, the stage's definition, the fight screen definition and the common files. The match description the scripts give (`external/script/replay.lua`): the kind of match (`lobby`, `match`, `netplay` or `local`), whether it was ranked, the game mode, the rounds and round time, which select.def character parameters were in use, the team modes, each side's characters in fight order (roster position, select.def entry, definition file, name and palette), whether it was a co-op match, the lobby (name, room ID, pairing) and, for an online match, the players (user ID, username, online name and tag, and their side; in a co-op match, member 1 or 2 of side 1) and whether the fight screen showed their names. |
| Session | One per stretch of rounds played without a character change: a Turns match has one per character change. The replay stream's header (see [Replay streaming](#replay-streaming)): IKEMEN's sync settings and content fingerprint, the random seed, the frame counters, the stage, the rules, the match context, the game parameters, the input slot each player read and each player's AI level, and whether the match was played offline (which the IsHost trigger reads: a local match has no host). For each player, its state when the session started: life, power, guard and dizzy points, red life, the character's variables and maps, and whether the CPU controls it. |
| Frames | The session's inputs, in the replay stream's chunk format, up to 600 frames per record. |
| End | The session's last frame. |

An arcade or survival run carries state from match to match: the score, the match number, consecutive wins, life in survival, and characters kept from the previous match with their variables. Each file records the state its match started with, so the replay of a run's third match plays by itself.

The file is gzip-compressed JSON lines, one gzip member per session, so that a file cut short (the game stopped while saving) keeps its complete sessions. The two players' files of an online match hold the same inputs, except the last few frames, which can differ: each game stops saving at its own last frame, which can include inputs it had predicted but not yet received from the other player when the match ended.

A server's replay of a ranked set (see [Watching a ranked set's replay](#watching-a-ranked-sets-replay)) has the same records, written as plain JSON lines without gzip, and holds every match of the set: each match record is followed by that match's sessions. The game reads both forms, and plays the matches of such a file one after another.

### Recording a local match

A local match is recorded so that its replay reads exactly what the match read:

- A recorded match runs at 60 frames per second, as online matches and their replays do; the Framerate setting does not apply to it.
- The controllers are read once per frame, when the match first reads one, and everything that reads a controller in that frame gets that reading, as in an online match. Button assist therefore applies per controller: characters sharing a controller (a Tag team) see the same assisted input.
- Pausing the match, with the pause menu, the Pause key or a frame step (Scroll Lock), is not part of the replay, at any game speed. A paused recorded match stands still: its clock holds, and it does not move on to the next round. The Lua code the game runs every frame (`Common.Lua`, which mods such as the rematch prompt hook into) waits while it is paused. When it resumes, the random numbers and the menus' reading of the controllers are as they were when it paused, so a button still held when it resumes counts as a new press. A frame step runs the match for one frame at the game's speed, which is more than one tick when the game runs faster than normal. A replay pauses and steps the same way.
- A speed set with the debug speed keys is reset to normal when a recorded match starts.

A change made to a match from outside it cannot be replayed, so recording stops at anything that can make one: any debug key or debug console command, loading a saved state, resetting the round or restarting the match from a menu, and changing the game speed, the input slots or a sync setting (for example from the pause menu's options). A character can make such a change too: resetting the round or restarting the match (the MatchRestart state controller, which does nothing during a replay), or loading a state saved with the F10 key rather than by a character. So can a script that pauses the match itself to wait for its players' answer (a mod's prompt), since a replay does not have the answer. The frames before the change are saved, the rest of the fight is not recorded, and the log names the reason.

The screens after a match (victory, results, continue) are not replayed. Lua code a fight is launched with (`launchFight`'s `lua` parameter) is not saved, since a replay file can come from anyone; a replay of such a fight runs without it. A game whose rollback sync test is on (`Netplay.Rollback.DesyncTestFrames` above 0) runs offline matches in the rollback loop and does not record them.

### Playing a match replay

- Each character is found by its select.def entry, or else by its definition file, which is added for the fight. A character or stage this game does not have stops the replay with a message.
- Files the match depends on that differ from the recording, and a different engine version, are listed first; the player chooses to play anyway (the replay may then play differently) or to go back.
- The recording's sync settings apply while it plays, as for a spectator, except the common files and code (see [Game compatibility](#game-compatibility)), which stay this game's. A replay whose strict settings, or common files and code, differ from this game's does not play.
- The rules recorded with each session (round time, ticks per timer count, rounds to win, draws), its match context and its game parameters replace the ones the screens set up. select.def character parameters, command-line flags or lobby settings that differ where the replay plays do not change them.
- Each player reads the input slot it read in the match: a ranked set's match played after the players switched sides replays each player's inputs on the side they played. The CPU's players get the AI levels they had, and every player starts each session with the state it had.
- The players' online names show on the fight screen when they did in the match (an online versus match).
- Holding a button plays at 4× speed. The Pause key pauses a replay without losing its place, and Scroll Lock then steps it one frame at a time. Esc leaves the replay; the players' own menu button, pressed in the match to pause it, does not.
- The victory and results screens are skipped.
- A file that holds several matches (a server's replay of a ranked set) plays them in order, until a match cannot play or the player leaves one with Esc. A match whose inputs stop before its end (a file cut short, or a Turns match whose next session is missing) counts as one that cannot play: the replay stops there and says why.

`Ikemen_GO -replay <file>` plays a match replay without the menus and quits: with exit status 0 when every match of the file played to its end, 1 otherwise (a match that could not play or stopped early, or one left with Esc). It writes its messages to the log instead of the screen, and plays content that differs from the recording without asking. `-replay-stats <file>`, given with it, writes the statistics of each match played (as `getGameStatsJson` returns them: the winning side, counted from 0, each round's winner and each character's life at its end) to that file as a JSON list.

A replay records the players' user IDs. Names and tags are kept as they were at the time, for display: results, ranked sets, lobby records and replays all tell players apart by their user IDs, so two players who share a name and a tag stay distinct.

### Session replays

The game also records the replay of a whole netplay session, from its first menu, as it did before match replays; delay-based netplay (Host Game / Join Game with the Netcode option on Delay-based) saves only that. When the session ends (`replayStop`), the file is removed if each of the session's matches has a match replay. A match has none when the scripts did not describe it (`replaySetMatchInfo`) or its file could not be written. A quick match from the command line (`-nakama-match`) is described when each side has one player, with no AI, input remapping or life and power flags; otherwise it is kept in the session replay only. Its players' names are filled in when the match is saved, since the server's answer can arrive after the fight began. The replay menu plays a session replay by replaying the netplay session from its first menu. What came from the server is not in it: the online rematch choices and the ranked set (see [Online rematch coordination](#online-rematch-coordination)). A match that a choice ended plays on to its own end, and the screen after a ranked set does not show; the replay picks up again at the next match.

A replay file can come from anyone, so playing one reads only files inside the game's folder and runs only this game's code: a character or stage is a definition file with a relative path, the files compared are regular files of at most 64 MB, and the common files and code are this game's. A file holds at most 64 sessions and three hours of frames; a longer online match is kept only in the session replay, and a longer local match is saved up to that length.

### Match replay Lua API

```lua
-- Saves each described rollback match of the netplay session that follows
-- to dir, one file per match; without dir, stops (replayStop also stops).
-- Raises an error when dir cannot be created. A session replay started with
-- replayRecord after this call is removed as saving stops, unless one of
-- the session's matches has no match replay.
replaySaveMatches("save/replays")
-- The next online match's description, saved as JSON. The scripts call it
-- just before each netplay fight (hook "start.f_game"); nil: the next match
-- is not saved as a match replay.
replaySetMatchInfo({kind = "lobby", selection = ..., players = ...})
-- Saves the next offline fight (every round and Turns character of the
-- game() that follows) to dir, described by info. Raises an error during an
-- online session or a replay, without a description, or when dir cannot be
-- created.
replayRecordLocal("save/replays", {kind = "local", selection = ...})
-- The file of the last match replay saved, online or local ("" for none).
local path = replayLastMatchFile()
-- nil for a replay of a whole session. Otherwise, for match n of the file
-- (default 1; a server's replay of a ranked set holds several), info (the
-- description), engine, this_engine, same_engine, date, sessions, frames,
-- stage, ai_levels (by player number), turns_offset (by side: how many
-- Turns members the fight skipped), match (n), matches (how many the file
-- holds) and content_listed (false when the file does not list the files
-- the match depends on: a server's replay of a match whose end did not
-- arrive); or nil and an error for a file that cannot be read.
local rp, err = replayMatchInfo(path, n)
-- The files match n depends on that differ in this game, given this
-- game's definition files of its characters and stage: a list of
-- {label, file, status}, status changed, missing or added (empty when the
-- file does not list them). Raises an error for a file that cannot be read.
local diffs = replayMatchContent(path, {p1 = {def1}, p2 = {def2}}, stageDef, n)
-- Starts playing match n (default 1); the scripts then load the fight it
-- describes (launchFight).
replayMatchPlay(path, n)
-- The user IDs set with the fight screen's names for the players now on
-- sides 1 and 2 ("" for none).
local u1, u2 = nakama.matchUsers()

-- external/script/replay.lua: the modes whose matches against the CPU are
-- saved (none by default), and the local versus modes saved when
-- Config.LocalVersusReplays is on.
replay.recordModes.arcade = true
replay.versusModes = {versus = true, versuscoop = true, challenger = true}
```

The messages about a replay that cannot play or differs from this game are `[Warning Info]` texts: `text.replayfailed.text`, `text.replaychars.text`, `text.replaystage.text`, `text.replaycontent.text`, `text.replayengine.text`, `text.replayunlisted.text`, `text.replaystopped.text` and the words they list (`text.replaystagefile.text`, `text.replayfightfile.text`, `text.replaychanged.text`, `text.replaymissing.text`, `text.replayadded.text`, `text.replaymore.text`).

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

-- One handler per event; a named handler is kept beside it and beside
-- other names: nakama.on(event, fn, name), removed with nakama.on(event, nil, name).
nakama.on("connected", function(event, data) end)
nakama.on("matchmaker_matched", function(event, data) end)
nakama.on("match_joined", function(event, data) end)
nakama.on("chat_message", function(event, data) end)
nakama.on("replay_chunk", function(event, data) end)
nakama.on("p2p_connected", function(event, data) end)
-- A notification from the server: data.payload has id, subject, code,
-- content (decoded: a table, or the text when it is not JSON), sender_id,
-- create_time and persistent. The ranked ones are described under Levels.
nakama.on("notification", function(event, data) end)

nakama.matchmake("ranked", 1000, 100)
nakama.matchmake("unranked")

-- Lobbies: the settings, state, events and commands are described under Lobbies.
nakama.createLobby({
    settings = {
        name = "Friday Night Fights",
        format = "winner_stays_on",
        max_games = 3,
        rounds = 3
    }
})
nakama.listLobbies()
nakama.findLobby("K7M2QX")
nakama.joinMatch(lobby_id, nil, {code = "K7M2QX"})

-- Nakama chat channels. Lobby rooms have their own chat (see Lobbies).
nakama.joinChat(room_name)
nakama.sendChat(channel_id, "hello")

-- Ranked sets (see Ranked sets). The engine keeps the set's state; the
-- scripts record each match's winner and claim a decided set's winner with
-- the server, once per set: submitRankedSet() returns true and the set's ID
-- when it sends the claim (false when the set is not decided, was claimed
-- already, or the game is not signed in). The engine sends the claim again
-- until the server's answer is final (see Claims sent again). Each answer
-- arrives as "ranked_result": data.payload has set_id, status, final (no
-- more sends), sends (how many so far) and the server's answer (with
-- result, see Claims and ratings); status is settled, disputed, void,
-- refused (the server refused the claim: data.error says why), open (the
-- other player's claim has not come), error (no answer: data.error) or
-- expired (a day old, no longer sent). A set that takes no more claims
-- refuses one that did not count with its status (settled, disputed or
-- void, with data.error and without the server's answer). The first version's call,
-- submitResult, is accepted for recorded ranked sets only, and answers
-- with "result".
-- rankedSet(): active, best_of, matches, p1_wins, p2_wins, p1_id, p2_id
-- (the set's players in the matchmaker's order), p1_side, p2_side, winner
-- (1 or 2, 0 until the set is decided), set (its number in the session)
-- and match_id (the ranked session's match id), among others.
local rs = nakama.rankedSet()
nakama.rankedSetRecordMatch(winner_side)   -- true when the match decides the set
local sent, set_id = nakama.submitRankedSet()
-- The claims that wait for a final answer: set_id, match_id, set, created
-- (Unix time), sends and next (seconds until the next send).
local waiting = nakama.rankedClaims()
-- Reads the claims an earlier run left in save/ranked_pending.json (the
-- first time) and sends every claim now; online.lua calls it at the start.
nakama.resumeRankedClaims()
nakama.submitResult(match_id, opponent_user_id, "my_game", 1, replay_hash)

-- The ready bit of a netplay session's menus (see The screen after a
-- ranked set). setReady raises or lowers this game's bit in the frames it
-- sends from now on (synchronize() lowers it); ready() is whether both
-- games' bits are up in the frame the menus run on now (ready(1) or
-- ready(2): the inputs read as player 1's or 2's), in a session or the
-- replay of one; sessionFrame() returns the session's frame number (-1
-- without one, or in a replay) and whether the menus still run in step with
-- the other game (false once it has left the session).
nakama.setReady(true)
local both = nakama.ready()
local frame, in_step = nakama.sessionFrame()
-- Ends the netplay session as Esc in its menus does (its screens close, and
-- the script that opened it calls exitNetPlay()); start.lua calls it for
-- Exit and Forfeit after a match.
nakama.endNetPlay()
-- The player's rating and level for a game (the profile's game by
-- default): emits "rating" with ikemen_get_rating's answer (see Levels).
nakama.getRating()

-- Online names (see Online names).
nakama.getAccount()
nakama.setAccountName("KAI")
nakama.setMatchNames("KAI", "ElPeleador", p1_user_id, p2_user_id)

-- Replay streams (see Replay streaming). How this client publishes the
-- streams of its next matches as the netplay host: enabled (default true),
-- delay in seconds (default Netplay.Rollback.ReplayBroadcastDelay) and info,
-- the match description sent to spectators. Without a table, the defaults.
nakama.setReplayPublish({enabled = true, delay = 3, info = {kind = "lobby", seq = 4}})
-- Replaces only the match description, for example once characters are chosen.
nakama.setReplayInfo({kind = "lobby", seq = 4, selection = selection})
-- The stream a spectator would start from: stream, segment, stage, match_info,
-- delay_frames, frame_rate, buffered_through, ready, ended, final_frame,
-- reset_reason.
local st = nakama.replayStatus()
-- Starts the live replay of that stream: true when it started, false while
-- the stream is not ready yet (the engine then shows its spectator loading
-- screen and starts the replay once it is, unless wait is false). The caller
-- sets up the fight, as for a replay file. Raises an error when no stream has
-- arrived, the stream was withdrawn, or this game's content fingerprint
-- differs from the header's.
nakama.startSpectatorReplay({wait = true})
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

The online lobby screens (lobby search, lobby settings and the lobby room) are drawn by `external/script/lobby.lua` from the screenpack's `[Lobby Info]` and `[LobbyBgDef]` sections. `[Lobby Info]` has built-in defaults, and a screenpack without `[LobbyBgDef]` gets its options or title background; the engine has no hardcoded lobby renderer. [lobby-screenpack.md](lobby-screenpack.md) describes the screens and their keys, and [Lobbies](#lobbies) describes how the server runs a lobby. A screenpack can also build its own lobby screens on the Lua API listed there.

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

The set result is tracked separately from GGPO rollback state. The choices end the match once it is over; a rollback session reports a match ended that way as quit, so the game takes the match's winner when the choice is applied (`external/mods/ik_rematch.lua`) and records it in the set. The netplay session goes on after such a match, with its menus in step (the screen after a ranked set, character select, a rematch), until Exit or Forfeit ends it (`nakama.endNetPlay`, in both games on the same frame). Each match starts without the previous match's choices. When a set has been decided, by its last match or by a forfeit, each player's game claims its winner with the server, which changes the two players' ratings once both claims agree (see [Ranked sets](#ranked-sets)), and both games show the screen after the set, which waits for the result (see [The screen after a ranked set](#the-screen-after-a-ranked-set)), before they go on. Starting another set resets the score and physical side assignment while retaining the same two matched players and negotiated ranked rules; the server records it as the session's next set. A new set does not carry forward winner-selection locks from the previous set.

The Nakama coordination match is retained between rounds; only the GGPO gameplay session is replaced. The P2P socket is also retained: each GGPO session opens a new channel on it and closes only that channel.

The previous versus-screen snapshot contains the resolved stage, team mode, character references, and palettes. A rematch reuses that snapshot and bypasses VS/order select. Ranked side-switching is applied after the completed match, so a rematch snapshot is transformed to keep each stable ranked-set player attached to their newly assigned physical side.

The server validates both rematch snapshots and requires them to agree before issuing the rematch decision. A malformed or mismatched snapshot falls back to Character Select.

The online menu returns through the existing `netplayversus` caller path; no separate hardcoded online-mode renderer is required.

### P2P primitive

The current Lua surface exposes:

```lua
nakama.startP2P(7500, "stun.example.net:3478")
nakama.stopP2P()
```

When the first argument is omitted, the engine uses `Netplay.Rollback.Port`. The port only matters to players who forward it; when it is taken (for example by a second instance on the same machine) the socket binds an ephemeral port instead. An empty STUN list uses the server profile's `stun_servers`.

This starts IPv4 host/server-reflexive candidate gathering and UDP hole punching. Signals carry the sender's and the intended peer's user ids; a lobby delivers each signal only to the member it names, and only the paired player answers. After the handshake the socket stays open until `nakama.stopP2P()`; the netplay stream and GGPO use channels on it (`src/nakama_p2pmux.go`). GGPO receives its channel through `transport.NewUdpFromPacketConn`, the constructor described in `patches/ggpo-nakama-transport.md`.

## Online sessions

`external/script/online.lua` takes two paired players from Nakama into IKEMEN netplay:

1. Connect to the profile's server and queue with `nakama.matchmake`.
2. When the matchmaker pairs the player, the engine joins the pairing's match. Both players start `nakama.startP2P` and wait for the handshake.
3. `nakama.enterNetPlay()` starts the netplay session over the P2P socket. The first user in the matchmaker's result hosts; Nakama sends the same order to both players.
4. From `connected()` on, the flow is the one Host Game and Join Game use: `synchronize()`, then the netplay menu. When the session ends, the player leaves the match and the P2P socket closes.

### Netcode

IKEMEN GO has two netcodes. With delay-based netcode, each game waits for the other's input before it runs a frame, so a slow connection delays every frame. With rollback netcode (GGPO), each game runs on at once with a guess of the other's input and corrects the frames it guessed wrong when the input arrives. The Netcode option (Options > Netplay Settings, `Netplay.RollbackNetcode`) chooses between them for Host Game and Join Game, and both players must have chosen the same one.

Online play through Nakama (quick match, ranked match and lobby matches) always uses rollback netcode, whatever each player's Netcode option says, and the two players' options are not compared. Its fights are always saved as match replays (see [Match replays](#match-replays)). The Options screen says so (Netcode (Online): Always Rollback). Host Game and Join Game are unchanged.

The fork's AI that plays CPU opponents with what it has learned from earlier fights by case-based reasoning (the CBR AI, see [About this fork](../README.md#about-this-fork)) never runs in a netplay session: it is inactive in every online match, ranked or not, and in Host Game and Join Game. It learns from online matches only through their replays, outside the session: a replay played from WATCH MODE > REPLAY, or a lobby match watched as a spectator (see [Watching a lobby match](#watching-a-lobby-match)), when the Learn From Replays option is on (as it is by default).

### The screen after a ranked set

In a ranked search, the searching screen also shows the player's level (`Power level 9,001`, see [Levels](#levels)), which the game asks the server for when the player queues.

After the last match of each ranked set, each game claims the set's winner with the server (see [Claims and ratings](#claims-and-ratings)), and a screen waits for the set's result. Between fights, the two games run a netplay session's menus in step: each frame runs on both players' input bits, which the games send each other, so both run the same frames on the same inputs. A screen that waits for something one game learns from the network cannot end when that one game learns it: the other game would go on alone, out of step. Each game has one more bit in what it sends, the *ready* bit, which is not a key and never acts as one. A game raises it once it has its final answer about the set: the server's answer to its claim, or the server's notification of the result (or at once, when it sent no claim). The screen ends on the first frame on which both games' bits are up, which both games see on the same frame, or after 600 frames (10 seconds), which both count alike. Until its own answer has come, it shows "Reporting the result..."; then "Waiting for the other player's result...".

Then each game shows its own result for 240 frames (4 seconds): who won the set and the player's level before and after it (`Power level 9,001 -> 10,840 (+1,839)`), that the result is disputed (or void, or refused), or, without an answer yet, "The result is still being reported; it will show when it arrives". If the other player leaves the session during the wait (the session ends), the game waits alone for its own answer, until 10 seconds have passed since the screen began. A result that comes later is printed to the log, and shows on the next screen the game draws itself, as "Last ranked set: ..." under the screen's own text: the ranked search screen, the menu with the online items, or the screen after the next set. It is not drawn over fights.

The texts are `online.text` entries of `external/script/online.lua` (`ranked_reporting`, `ranked_waiting_other`, `ranked_pending`, `ranked_won`, `ranked_lost`, `ranked_level`, `ranked_disputed`, `ranked_void`, `ranked_refused`, `ranked_last` and others), which a screenpack or module can replace; the engine's default font for these screens (`f-6x9`) draws ASCII only, hence `->` for the arrow. `online.rankedWaitFrames`, `online.rankedWaitSeconds` and `online.rankedResultFrames` hold the lengths. A session's replay (see [Session replays](#session-replays)) records the ready bits with the inputs, and they do not act as keys when it plays either; its playback does not show this screen, since a replay has no ranked set (nor the online rematch choices, which come from the server).

A screenpack adds the entry points as title menu items (the third one opens the lobby screens, see [Lobbies](#lobbies)):

```ini
menu.itemname.menunetwork.onlinematch = QUICK MATCH (ONLINE)
menu.itemname.menunetwork.onlineranked = RANKED MATCH (ONLINE)
menu.itemname.menunetwork.onlinelobby = ONLINE LOBBIES
```

For testing, `-nakama-match unranked` (or `ranked`) with the quick-VS options (`-p1`, `-p2`, `-s`) does the same as `-ip`, with the opponent found by matchmaking. `-nakama-stun host:port,...` overrides the STUN list. A ranked quick match plays one match, the first of its set: with `Netplay.Ranked.BestOf = 1` that match decides the set, and the game claims the set's winner with the server and shows the screen after the set (with `-loadmotif`; without it the window stays blank), which waits up to 10 seconds as in the menus and then shows the result for 4 seconds (`online.rankedResultFrames`), so the game quits up to about 14 seconds after the match; it prints the set's result (`Ranked set <set ID>: You won the ranked set. Power level 9,001 -> 10,840 (+1,839)`) when it has come. A claim still without a final answer goes again at the game's next start. A longer set is not claimed: the player who quits first while the other is still in the session is noted as having left early, and once `IKEMEN_RANKED_CLAIM_MINUTES` have passed the set is disputed (`claim_timeout`), so it shows in the moderators' list (see [Ranked sets](#ranked-sets)).

Lua API used by the flow, also used by the lobby screens:

```lua
local st = nakama.status()
-- st.connected, st.matchmaking, st.match_id
-- st.p2p: none | connecting | ready | failed | closed
-- st.netplay: none | connecting | connected | failed
-- st.host and st.peer once the pairing is known; st.error, st.netplay_error
-- st.rtt_ms: last server round trip measured by nakama.ping() (see Lobbies), -1 until known
-- st.region, st.game, st.build: from the server profile

nakama.setPairing(peer_user_id, is_host) -- a lobby pairing, before startP2P
nakama.enterNetPlay()                    -- or enterNetPlay(is_host)
nakama.clearError()
```

A lobby match takes the same steps with the pairing the lobby announces; [Lobbies](#lobbies) describes it.

Connection behaviour:

| Situation | Result |
|---|---|
| Both players behind typical home routers (NAT with a WAN firewall) | Connects |
| One player behind such a router, the other with a public address | Connects |
| One player behind a symmetric NAT (random port per destination), the other behind a port-restricted NAT | Fails after about 20 seconds with "Could not open a connection to the other player" |
| Both players behind conntrack NATs without a WAN firewall | Fails the same way: the first player's packets reach the other NAT before that player has sent anything, and the NAT then moves the other player's mapping to a new port |
| A player quits or crashes during the session | The other side's session ends within about 8 seconds (1 second when the player leaves normally) |
| The server stops, or the connection to it fails, during the session | The session goes on over the P2P path, which the game keeps with the session's match, so that both games take the same steps whether one or both lost their connection (the next fight runs over the P2P path too). The online rematch choices go through the server: a game without the connection cannot send its choice after a match, and the decision does not reach it. Claims of ranked sets are sent again (see [Claims sent again](#claims-sent-again)). The session's end leaves the match. |

A relay (TURN, or relaying through Nakama) is needed for the failing cases; none is implemented yet.

## Ranked sets

A ranked session is the session the matchmaker makes for two players who queued for a ranked match (`nakama.matchmake("ranked")`, RANKED MATCH in the online menu). Its players play best-of-N sets: `Netplay.Ranked.BestOf` matches (3 by default; an even number becomes the next odd one, and 99 is the most), with the side switching and winner-keeps-selection rules of `Netplay.Ranked.SwitchSides` and `Netplay.Ranked.WinnerKeepsSelection`. Both players' tickets carry these rules, and their queries pair only tickets that agree on them and on the game, its build and the region; the server makes an unranked session of a pair whose tickets differ in game, build or set length (which can meet when the server checks only one ticket's query, Nakama's `matchmaker.rev_precision` being false by default), and the games then play it as an unranked session. Ratings change once per set, never per match: a set is one rated result.

The server does not watch the fight. It learns a set's result from the players' games, and keeps both players' replays of the set so that its moderators can watch the set when the players disagree. It does not replay them itself.

### The set's record

When the matchmaker makes a ranked session (`ikemen_matchmaker.lua`), the server records the session's first set: the game, the build, the set's length (`best_of`), the two players (user ID and username) in the matchmaker's order, which every player receives too (the first player hosts the netplay session and starts on side 1), the time it was created, and the status `open`. When both players choose Play Another Set (see [Online rematch coordination](#online-rematch-coordination)), the session records the next set. A set's ID is the session's match ID for its first set, and `<match ID>#<n>` for its n-th set.

| Status | Meaning |
|---|---|
| `open` | The set is being played, or waits for its players' claims. |
| `settled` | Both claims named the same winner, or a moderator settled the set: the ratings changed, once. |
| `disputed` | The claims named different winners (`reason`: `claims_disagree`), or a claim did not come in time (`claim_timeout`): only one player claimed, or nobody claimed after a player had left a set whose matches had started. A moderator decides. |
| `void` | No result: nobody claimed in time, and either nobody left before the set was decided or none of its matches had started (`no_claims`); or a moderator voided it (`moderator`). The ratings do not change. |

The record also notes the claims, the players' results once the set has settled (`results`, by user ID: `won`, `label`, `rating_before`, `rating`, `level_before` and `level`, the Elo ratings and levels before and after the set, the levels and their label as the server's level settings made them then), a player who left the session before the set was decided (`left_early`: such a set ends disputed when only the other player claims it, and a moderator decides; the session counts a set as decided when a player's choice after a match says it was the set's last, when a player forfeits, when both choose another set, or once both have claimed it, so this is the games' account, not proof), whether a match of the set started (`played`), when the set ended (the end of the session, or for an earlier set, the start of the next one), the replays kept, the winner, who settled it (`players` or `moderator`) and a moderator's note.

### Claims and ratings

When a set has been decided, by its last match or by a forfeit, each player's game claims its winner (`nakama.submitRankedSet`, called by `external/script/start.lua`; it sends the claim again until the answer is final, see [Claims sent again](#claims-sent-again)), with the `ikemen_submit_result` RPC and the player's session:

```json
{"match_id": "<the session's match ID>", "set": 1, "winner": "<the winner's user ID>"}
```

The answer is `{accepted, settled, status, match_id}`, with, once the set is settled, the caller's `rating`, `level`, `level_text` and the game's `label`, the opponent's `opponent_rating`, `opponent_level` and `opponent_level_text` (see [Levels](#levels)), and `result`: what the set changed for the caller, `{won, label, rating_before, rating, level_before, level_before_text, level, level_text, change, change_text}`, the fields of the `ikemen_ranked_settled` notification. A claim sent again after the set was settled gets the caller's rating and level as they are now, without the opponent's, and the same `result`, which the set's record keeps (with the label its levels had, should the level settings change later); a claim sent again after the set was disputed gets its `reason`. So a game whose first answer was `open`, or that had no answer, learns the set's result from a claim it sends again, without the notification. A set settled before the server kept results has no `result`. The server accepts a claim only for a set it recorded, from one of the set's two players, while the set is open, and refuses the others with these Nakama error codes:

| Code | Refused |
|---|---|
| 3 `INVALID_ARGUMENT` | A field is missing or does not fit the set: no `match_id`, or a `winner` who is not one of the set's players. |
| 5 `NOT_FOUND` | The server made no ranked session with this ID, or has no record of the set: the session has no such set, or the server could not record it. |
| 7 `PERMISSION_DENIED` | The caller was not one of the set's players. |
| 9 `FAILED_PRECONDITION` | The match is an unranked session or a lobby, the set is settled, disputed or void, or the caller has already claimed the other winner. |
| 10 `ABORTED` | The set's record kept changing meanwhile; the claim can be sent again. |
| 16 `UNAUTHENTICATED` | No player's session. |

A claim sent again with the same winner is answered as the first was. When both claims name the same winner, the set settles: one storage transaction writes a settlement marker (`ikemen_settlement/<set ID>`, only if it does not exist yet), both players' new Elo ratings and the set's record, so claims sent at the same time, or a claim and a moderator's settlement at the same time, change the ratings once. Then the server writes both players' entries on the game's leaderboard and sends each player a notification with their level before and after the set (see [Levels](#levels)); neither can undo the settlement, and their failures are logged. When the claims differ, the set is disputed, and both players are told so. The second claim is due within `IKEMEN_RANKED_CLAIM_MINUTES` (15 by default) of the set's end; the server checks this when it next reads the set (for a claim or for the moderators' list), and a set whose session never reported its end (the server stopped meanwhile) times out 24 hours after it was created.

The ratings are Elo ratings per game (`ikemen_rating/<game>`, one record per player): 1000 to start with, a K-factor of 32 and a scale of 400 (`GAME_CONFIG` in `ikemen_rating.lua` changes them per game; the matchmaker reads the ratings with the same settings). `games`, `wins` and `losses` count sets. The matchmaker pairs ranked players by the rating stored on the server, not by the one the client sends. Players see a level computed from their rating, which each server sets (see [Levels](#levels)).

The first version's fields still work: `result` (1: the caller won, 0: the caller lost) in place of `winner`, `opponent_id` and `game` (checked against the set when given) and `replay_hash` (claims whose hashes differ disagree).

Two players who agree on a false result are believed: agreeing claims prove only that both games, or both players, say the same.

### Claims sent again

An answer is final when it says what became of the set: `settled`, `disputed`, or a refusal (codes 3, 5, 7 and 9). A code 9 for a set that is no longer open (`ranked set <set ID> is <status>: it takes no more claims`, the answer to a claim that did not count) means its status: `void`, `disputed` or `settled`, without the set's result; the game then shows the line of that status (for a settled set, that it is settled), unless the server's notification brought the result. `open` is not final (the other player's claim has not come), and neither is a claim the server did not answer: no connection, the server stopped, or codes 10, 13, 14 and 16. A claim without a final answer is kept by the game (`src/nakama_ranked_queue.go`), in memory and in `save/ranked_pending.json`, and sent again: 5 seconds after the first send, 15 seconds after the second, then every 60 seconds; at once whenever the game connects to the server again (`nakama.connect`); and when the game next starts. It is sent until its answer is final, or until it is a day old; once the server's claim window (`IKEMEN_RANKED_CLAIM_MINUTES`) has passed, the server's answer is final (the set is disputed or void by then). A claim is sent only as the account that played the set: after a restart, the game signs in to the set's server with the account's device ID on its own, over the server's HTTP API, without the realtime connection (the player is not shown online).

The file:

```json
{"version": 1, "claims": [{"set_id": "<set ID>", "match_id": "<the session's match ID>", "set": 1,
  "winner": "<the winner's user ID>", "user_id": "<the claiming player's user ID>",
  "server": {"host": "127.0.0.1", "port": 7350, "server_key": "defaultkey", "device_id": "..."},
  "created": 1792112236, "sends": 3}]}
```

`created` is the time of the first send (Unix seconds) and `sends` the number of sends so far. `"ssl": true` is there only for a server reached over TLS, and `device_id` only when the account's device ID came from the server profile (`device_id`), not from `save/nakama_device_id`. The file is removed when no claim waits. A file that cannot be read is ignored, and replaced at the next change.

### Replays kept for moderators

In a ranked session both players' games publish their replay stream (see [Replay streaming](#replay-streaming)), and the session keeps both streams instead of relaying them: for each player, every stream the player's game published during the set, with their headers, inputs and ends. The session stores a player's replay of a set (`ikemen_ranked_replays/<set ID>:<user ID>`) when the set has been decided and the player's last stream of it has ended, when the player leaves the session, and when the session ends. The set's record lists each stored replay with its size and whether it is complete: the set was decided, every stream ended, the player did not leave before the set was decided, and nothing was left out for the size limit. The two players' replays of a set hold the same inputs; the second one is there for a set whose first replay is incomplete.

The session drops a stream message larger than its limit (a header or an end over 32 KB, a chunk over 8 KB). It keeps a stream only as it continues: each chunk must hold the frames that follow the ones kept (at most 30, each with its inputs), and the end must put the stream's end where those frames end; otherwise the stream ends where it was. It keeps at most `IKEMEN_RANKED_REPLAY_MAX_BYTES` per player and set (16 MiB by default, about 50 minutes of play), and no faster than play makes them: over all the session's sets, at most 256 KiB of a player's streams plus 16 KiB for each second the session has lasted (play publishes about 5.3 KB a second, and about 9 KB more per match). Past either limit, the stream ends at its last frames kept, the rest of the player's replay of the set is left out, and the replay is incomplete. A replay is kept for `IKEMEN_RANKED_REPLAY_DAYS` days (30 by default). Expired replays are removed lazily: up to 20 whenever a replay is stored or the moderators' list is read, and an expired replay that a moderator asks for. The set's record stays.

### Ranked settings

The ranked settings are Nakama runtime environment values (`runtime.env` in Nakama's configuration, or `--runtime.env NAME=value` on its command line). Fractions are allowed.

| Setting | Default | Meaning |
|---|---|---|
| `IKEMEN_RANKED_CLAIM_MINUTES` | 15 | Minutes after the set's end within which the second claim must come; later, the set is disputed (or void when nobody claimed). |
| `IKEMEN_RANKED_REPLAY_DAYS` | 30 | Days a replay is kept. 0 keeps none: the sessions do not keep the streams. |
| `IKEMEN_RANKED_REPLAY_MAX_BYTES` | 16777216 | The most bytes of stream messages kept per player and set. |

The levels that players see for their ratings are another runtime environment value, `IKEMEN_LEVELS` (see [Level settings](#level-settings)).

### Storage

Every record of the ranked sets is system-owned, with read and write permission 0: players can neither read nor change them through Nakama's storage API, and the modules read only the system-owned records.

| Collection | Key | Holds |
|---|---|---|
| `ikemen_ranked_sets` | The set ID | The set's record. |
| `ikemen_ranked_set_index` | A 13-digit number that counts down with the time the set was recorded (in milliseconds), `:`, the set ID | The sets, newest first, for the moderators' list. |
| `ikemen_ranked_replays` | `<set ID>:<user ID>` | A player's replay of the set: the stream messages as they came, the size, whether it is complete, and when it expires. |
| `ikemen_ranked_replay_expiry` | The expiry time (Unix seconds, 12 digits), `:`, a hash of the replay's key | When each replay expires, oldest first. |
| `ikemen_settlement` | The set ID | The settlement marker: the set's players, game and winner. |
| `ikemen_rating` | The game | A player's rating: owned by the player, who can read it but not write it (written by the server). |

### Moderator RPCs

`ikemen_moderation.lua` registers three RPCs for the server's operators. They are called server to server, with Nakama's runtime HTTP key (`runtime.http_key`), and refuse any call made with a player's session (code 7). Over HTTP, with the payload as the request body:

```text
curl "http://127.0.0.1:7350/v2/rpc/ikemen_ranked_list?http_key=<runtime.http_key>&unwrap" -d '{"status": "disputed"}'
```

With `unwrap`, the request body is the payload and the answer is the RPC's JSON answer; without it, Nakama expects the payload as a JSON string and returns `{"payload": "<the answer as a string>"}`. An error answers with an HTTP error status and `{"code", "message"}`: 3 `INVALID_ARGUMENT` (a field is wrong), 5 `NOT_FOUND` (no such set, or no replay kept), 7 `PERMISSION_DENIED` (a player's session), 9 `FAILED_PRECONDITION` (the set is settled or void already) or 10 `ABORTED` (the set kept changing; call again).

**`ikemen_ranked_list`** `{status, limit, cursor, match_id}` → `{sets, cursor}`

- `status`: `disputed` (the default), `open`, `settled`, `void` or `all`. `limit`: 1 to 100 sets (20 by default). `cursor`: the `cursor` of the previous answer, for the next page; it is `""` after the last page. A page can hold fewer than `limit` sets before the last page (one call reads at most 50 pages of the index).
- `match_id`, a set ID: the list holds that set alone (or nothing), whatever its status unless `status` is given.
- A set whose second claim is overdue is marked disputed (`claim_timeout`) or void when the list reads it (see [Claims and ratings](#claims-and-ratings)); its players are told of a dispute then (see [Levels](#levels)).
- Newest first. Each set: `match_id` (the set ID, which the other RPCs take), `session` (the session's match ID), `set` (its number in the session), `game`, `build`, `best_of`, `status`, `reason`, `players` (`[{user_id, username, rating, level, level_text, result}]`, in the matchmaker's order, with each player's current rating and level, and, once the set has settled, `result`: the player's result in the set as the record kept it, `{won, label, rating_before, rating, level_before, level_before_text, level, level_text, change, change_text}`, see [Claims and ratings](#claims-and-ratings)), `label` (the name of the game's level), `claims` (`[{user_id, winner, at}]`), `left_early` (user IDs), `replays` (`[{user_id, bytes, complete}]`), `played`, `created`, `ended`, `decided` (when the status last changed), `winner`, `settled_by` and `note`. Times are Unix times in seconds; `ended`, `decided`, `winner` and `settled_by` are left out until they apply.

**`ikemen_ranked_replay`** `{match_id, user_id}` → `{file_name, replay, user_id, complete, matches, bytes}`

- `match_id`: the set ID. `user_id`: whose replay (optional): without it, the first complete replay in the players' order, or else the longest.
- `replay` is a match replay file (see [Match replays](#match-replays)) encoded in base64, which this game plays (see [Watching a ranked set's replay](#watching-a-ranked-sets-replay)); `file_name` names it as the game names its replays, for example `2026-10-13_00h30m48s KAI vs ElPeleador (ranked a05f6f6b, KAI).replay`. `user_id` and `complete` describe the replay, `matches` is the number of the set's matches it holds, and `bytes` the size of the file.

**`ikemen_ranked_settle`** `{match_id, winner, void, note}` → `{status, match_id, winner}`

- Either `winner`, the user ID of one of the set's players, which settles the set for that player with the same rating change, settlement marker, leaderboard entries and notifications as the players' claims, or `void: true`, which voids it without a rating change or a notification. `note` (optional, at most 500 characters) is kept on the record.
- `status` is `settled` or `void`. A set that is settled or void already is refused (code 9); an open or disputed set can be decided.

### Watching a ranked set's replay

A moderator saves the decoded `replay` in the game's `save/replays` folder under its `file_name`, and plays it from WATCH MODE > REPLAY, or with `Ikemen_GO -replay <file>` (see [Playing a match replay](#playing-a-match-replay)). The file plays the set's matches one after another, each with the players' characters, stage, rules and online names, and the players on the sides they played; each match's description in the file (its `players`, with their sides) tells which side each player played. The moderator's game should be the set's game and build (`game` and `build` on the record), with the set's characters and stage (found by their select.def entry or their definition file). Files that differ from the players' are listed before the replay plays: with them, a match may play differently from the set. With the same content, each match plays as it did in the players' games, so the moderator sees who won it.

For example, with `curl` and `jq`:

```text
curl -s "http://127.0.0.1:7350/v2/rpc/ikemen_ranked_replay?http_key=<key>&unwrap" -d '{"match_id": "<set ID>"}' > answer.json
jq -r .replay answer.json | base64 -d > "save/replays/$(jq -r .file_name answer.json)"
```

## Levels

Each player keeps an ordinary Elo rating per game (see [Claims and ratings](#claims-and-ratings)): it starts at 1000, the matchmaker pairs ranked players by it, and each ranked set changes it, by 16 between two players of the same rating. What players see is a level computed from it, which each server's creator sets: for example, a community where every new player has a power level of 9,001 and the best players reach the billions. Without settings, the level is the Elo rating itself, labelled `Rating`, as servers showed before.

### The formula

```text
level = start × (top ÷ start) ^ ((Elo − 1000) ÷ 1000)
```

`start` is the level of a new player, whose rating is 1000 (with another starting rating in `GAME_CONFIG`, the formula measures from that one), and `top` the level of a player 1000 Elo above a new one, which in a large community is the very top. Each step of Elo multiplies the level by the same factor, so a level is always positive, however low the rating. Levels are rounded to whole numbers, at least 1 and at most 9,000,000,000,000,000: up to there, the server's numbers (64-bit floating point) hold every whole number exactly. The settings only map ratings to levels: changing them changes every player's level at once, and no stored rating.

With `start` 9001 and `top` 1,000,000,000:

| Elo | Level |
|---|---|
| 0 | 1 |
| 600 | 86 |
| 984 (a new player who lost a set to another) | 7,474 |
| 1000 (a new player) | 9,001 |
| 1016 (a new player who won a set against another) | 10,840 |
| 1100 | 28,765 |
| 1500 | 3,000,167 |
| 2000 | 1,000,000,000 |
| 2200 | 10,212,729,937 |

### Level settings

The levels are set by `IKEMEN_LEVELS`, a runtime environment value like the [ranked settings](#ranked-settings), whose value is a JSON object:

```json
{
  "default": {"start": 9001, "top": 1000000000, "label": "Power level"},
  "games": {
    "my_other_game": {"start": 100, "top": 10000, "label": "Rank"},
    "my_third_game": {"label": "Score"}
  }
}
```

| Field | Meaning |
|---|---|
| `default` | The levels of every game without an entry of its own. Without it, the plain Elo ratings, labelled `Rating`. |
| `games` | Entries of their own, by game ID: the `game` of the game's server profile (see [Shipped client server profiles](#shipped-client-server-profiles)), which the game sends in its tickets. A game ID has 1 to 114 bytes, so that the game's leaderboard ID (see [Leaderboards](#leaderboards)) fits in Nakama's 128. |
| `start`, `top` | Both, or neither. `start` is a number of at least 1, `top` a number more than `start` and at most 9000000000000000. A game's entry without them takes the default's. |
| `label` | The name of the number, as players see it before the level: 1 to 32 characters, none of them a control character (surrounding spaces are removed). A game's entry without one takes the default's, and the default's is `Rating`. |

Each field is optional, and any other field is an error. In Nakama's YAML configuration file, the value goes in single quotes:

```yaml
runtime:
  env:
    - 'IKEMEN_LEVELS={"default": {"start": 9001, "top": 1000000000, "label": "Power level"}}'
```

A single quote inside the value is written twice, as YAML requires: a label `Saiyan's power` is written `"label": "Saiyan''s power"`. A single quote written once ends the value early, and Nakama does not start (`Could not parse config file`).

The server reads the value when it starts, and logs the levels in effect:

```text
ikemen levels: levels are start 9001, top 1000000000, labelled "Power level"; game "my_other_game": start 100, top 10000, labelled "Rank"; game "my_third_game": start 9001, top 1000000000, labelled "Score"
```

A value that is not valid is logged as an error, with what is wrong, and the server shows the plain Elo ratings for every game:

```text
ikemen levels: IKEMEN_LEVELS is not valid (default: top must be more than start): levels are the plain Elo ratings, labelled "Rating", for every game
```

### Where players see them

The server writes each level as text too (`level_text`), with thousands separators (`9,001`), so that every client shows the same text; levels reach billions and more, past what a 32-bit integer holds, and a client that reads them as numbers needs 64-bit ones. IKEMEN GO shows the player's level while it searches for a ranked match, and the set's result after each ranked set (see [The screen after a ranked set](#the-screen-after-a-ranked-set)). Each game's leaderboard lists the players by rating, with their levels, for a launcher to show (Nakama Matchmaker's does).

These answers carry the levels:

- **`ikemen_get_rating`** `{game}` → `{rating, games, wins, losses, level, level_text, label}`: the caller's Elo `rating`, the ranked sets they played, won and lost, their `level` as a number and as text, and the `label` of the game's number. A player without a rating has the rating new players start with. When the caller has played a ranked set and their leaderboard entry is missing or older than their rating (a rating from before the leaderboards, or a write that failed), it writes the entry. A payload without `game` is refused with code 3.
- The answer to a claim, once the set is settled, with the caller's level before and after the set (see [Claims and ratings](#claims-and-ratings)).
- The moderators' list of sets, with each player's current level, and their levels before and after a settled set (see [Moderator RPCs](#moderator-rpcs)).

### Leaderboards

Each game has a leaderboard: the Nakama leaderboard `ikemen_rating.<game ID>`, created when the game's first ranked set settles. It is authoritative: only the server writes it, and Nakama refuses players' writes and deletes (code 7). A player's record has their rating times 1000, rounded to a whole number (at least 0), as its score and the sets they won as its subscore, highest score first, so that the order follows the levels, which come from the exact rating; its metadata holds the rating itself, `wins`, `losses` and `games`. When a set settles, the server writes both players' records. A player whose rating dates from before the leaderboards appears after their next set, or when their game next asks for their rating (`ikemen_get_rating`, which IKEMEN GO calls when the player queues for a ranked match). Levels are computed when the leaderboard is read.

**`ikemen_leaderboard`** `{game, limit, cursor}` → `{label, records, cursor, mine}`

Called with a player's session, or server to server with the runtime HTTP key, like the [moderator RPCs](#moderator-rpcs).

- `game`: the game ID. `limit`: 1 to 100 records (20 by default). `cursor`: the `cursor` of the previous answer, for the next page; it is `""` after the last page.
- `records`: `[{rank, user_id, name, level, level_text, wins, losses}]`, highest rating first, `rank` counting from 1. `name` is the player's online name (see [Online names](#online-names)): the account's display name, or its username until the player chooses a name, at most 16 characters.
- `mine`: the caller's own record, with its rank, whichever page is asked for; left out when the caller has no record, or the call has no player's session.
- `label`: the name of the game's number. A game none of whose sets has settled has no records.
- Errors: code 3 `INVALID_ARGUMENT` for no `game`, a game ID over 114 bytes, or a `limit` out of range.

```text
curl "http://127.0.0.1:7350/v2/rpc/ikemen_leaderboard?http_key=<runtime.http_key>&unwrap" -d '{"game": "my_game", "limit": 2}'
```

```json
{"label": "Power level", "cursor": "<the next page's cursor>",
 "records": [{"rank": 1, "user_id": "<user ID>", "name": "KAI", "level": 12943, "level_text": "12,943", "wins": 2, "losses": 0},
             {"rank": 2, "user_id": "<user ID>", "name": "ElPeleador", "level": 9153, "level_text": "9,153", "wins": 1, "losses": 1}]}
```

### Notifications

When a set settles, by its players' claims or by a moderator, the server sends each of its two players a Nakama notification with their level before and after the set. When a set becomes disputed, it tells both players that a moderator will decide; that moderator's settlement then sends the settled notification. A void set sends nothing. The notifications are persistent: a player who was not connected finds them in Nakama's notification list (`GET /v2/notification`). IKEMEN GO receives them as the `notification` event (see [Lua API](#lua-api)).

| Subject | Code | Content |
|---|---|---|
| `ikemen_ranked_settled` | 4101 | `match_id` (the set ID), `session` (the session's match ID), `set` (its number in the session), `game`, `won` (whether this player won the set), `settled_by` (`players` or `moderator`), `label`, `rating_before`, `rating`, `level_before`, `level_before_text`, `level`, `level_text`, `change` (`level` − `level_before`), `change_text` (with its sign: `+1,839`, `-1,527`, `+0`), `wins` and `losses` (the player's sets, this one included). |
| `ikemen_ranked_disputed` | 4102 | `match_id`, `session`, `set`, `game` and `reason`: `claims_disagree` or `replay_hash_disagree` (the claims disagree), or `claim_timeout` (the second claim did not come in time, which the server finds when it next reads the set, see [Claims and ratings](#claims-and-ratings)). |

For example, the winner of a first set between two new players, on a server with the settings above:

```json
{"match_id": "<set ID>", "session": "<set ID>", "set": 1, "game": "my_game", "won": true, "settled_by": "players",
 "label": "Power level", "rating_before": 1000, "rating": 1016, "level_before": 9001, "level_before_text": "9,001",
 "level": 10840, "level_text": "10,840", "change": 1839, "change_text": "+1,839", "wins": 1, "losses": 0}
```

The leaderboard entries and the notifications follow the settlement's storage transaction and cannot undo it: when one fails, the server logs the failure, and the settlement stands.

## Online names

A player's online name is the account's display name (Nakama's `display_name`). Lobbies show it, and the fight screen shows it in place of the name of the character the player controls (see [Online names on the fight screen](#online-names-on-the-fight-screen)). Names need not be unique: the server and the scripts tell accounts apart by their user IDs, and lobbies show each member's tag beside the name, four digits taken from the account ID (for example `KAI (7841)`), so that members with the same name can be told apart. A tag identifies an account for other players; it does not prove it, since new accounts are cheap.

A name has 2 to 16 characters and no control characters; surrounding spaces are removed. Which characters a game accepts is up to its lifebar: the lobby screens accept a name only when the fight screen's name fonts can draw every character of it (`nakama.nameMissing`), in the name element of each team mode's layout, since the name stands in for a character's name there. A game whose lifebar font has capitals only therefore accepts names in capitals only. Lifebar and screenpack fonts are usually bitmap fonts, which draw printable ASCII only; [lobby-screenpack.md, Fonts](lobby-screenpack.md#fonts) describes what the lobby's fonts must cover.

The server's `ikemen_account_name` RPC (`nakama/modules/ikemen_account.lua`) checks the length and the control characters and stores the name as the account's display name. It fails with Nakama error code 3 for a name that breaks these rules and 13 when the account could not be updated. Nakama's own account update (`PUT /v2/account`) can also set a display name; a before-hook in the same module holds it to the same rules. The server cannot check the lifebar's characters, which only the game knows. A name with characters a game's lifebar cannot draw (stored by a modified client, or chosen in a game with another lifebar) shows the character's name on that game's fight screen, and the lobby screens leave those characters out.

A lobby reads a member's name from the account when the member joins, so a new name shows in the next lobby joined. An account without a display name has never chosen a name: lobbies show its username, cut to 16 characters; the fight screen shows the character's name; and the lobby screens ask for a name on the first visit to lobby search with that account in a session. The NAME item of lobby search changes it later. When an account is created, before-hooks on Nakama's authentication leave out a username that Nakama would refuse (one taken by an account, over 128 bytes, or with a control character or the pair `[]`), so that Nakama generates one instead of refusing the sign-in. An email sign-in without an email address, which signs in to an existing account by username and password, keeps its username.

```lua
-- Emits "account" with payload user_id, username and display_name (empty
-- until a name has been chosen), or with error.
nakama.getAccount()
-- Emits "account_name" with payload.display_name, or with error and
-- payload.reason: invalid (the server refused the name) or failed.
-- payload.name is the name asked for, which tells a late reply to an
-- earlier request from this one.
nakama.setAccountName("KAI")
-- The online name as last read or set ("" before either), and the username.
nakama.displayName()
nakama.username()
-- The characters of a name that the lifebar's name fonts cannot draw, each
-- once and separated by spaces ("" when the lifebar can show the name);
-- characters outside ASCII are written as code points.
nakama.nameMissing("Kaié")   --> "U+00E9"
-- Reads other accounts. Emits "users" with payload.users, a list of
-- user_id, username and display_name, or with error.
nakama.getUsers({user_id1, user_id2})
```

### Online names on the fight screen

During an online match, the fight screen shows each player's online name in place of the name of the character that player is playing as: in the first name slot of each side, which holds the current character in Turns and the leader in Simul and Tag (a partner who tags in becomes the leader). The name uses that slot's element and font in the lifebar. The other name slots, the list of Turns characters still to come, and the versus and victory screens keep the characters' names, and characters keep their own names for their code (`ModifyPlayer` `lifebarname`, name triggers). A player without an online name shows the character's name, and so does a name the slot's font cannot draw completely.

The names are display state only and never part of the rollback state. The online scripts set them for each online match: the lobby screens from the lobby's members (the pairing's player 1 is on side 1), and online matchmaking from the two accounts (`nakama.getUsers`; the matchmaker's first player hosts and starts on side 1). With the players' user IDs, a ranked set that switches sides moves the names with the players. The names are per side, so they show in a fight between the two players; a co-op fight of a matchmade session (both players on side 1, the CPU on side 2) keeps the characters' names. A lobby match's replay stream carries the names, so its spectators see them too. Names show only in netplay, live replays and match replays (which record them, see [Match replays](#match-replays)), so names left set cannot reach an offline match.

```lua
-- The names for the players on sides 1 and 2 ("" keeps the character's
-- name), with their user IDs when known; without arguments, clears them.
nakama.setMatchNames("KAI", "ElPeleador", p1_user_id, p2_user_id)
nakama.setMatchNames()
-- The names set for the players now on sides 1 and 2 ("" for none).
local p1, p2 = nakama.matchNames()
```

## Lobbies

A lobby is an authoritative Nakama match (`nakama/modules/ikemen_lobby.lua`) where up to eight members gather and play one-on-one matches in turn. The lobby keeps the members, their ready states and the settings, and decides who plays whom; it does not run any gameplay. The two players of a pairing open a P2P netplay session between themselves with the steps of [Online sessions](#online-sessions), play one match, report the result to the lobby and return to the lobby room. Lobby matches do not offer the rematch choices of [Online rematch coordination](#online-rematch-coordination).

A member's client receives the lobby's state whenever it changes: the members, the phase, the settings, the pairing being played with its sequence number `seq`, and the next pairing. It also receives events (chat messages, notices, errors), and it acts by sending commands (`ready`, `chat`, `settings`, `start`, `kick`, `host`, `member`, `link`, `watching`, `watch`, `pair_failed`, `no_contest`) and match results. Members who are not playing can watch the match being played ([Watching a lobby match](#watching-a-lobby-match)). [Lobby Lua API](#lobby-lua-api) lists the state, events and commands.

The lobby screens that players use (`external/script/lobby.lua`) are described in [lobby-screenpack.md](lobby-screenpack.md). This section describes the lobby itself, for those screens and for any other lobby UI.

### Members, host and room IDs

Members are listed in the order they first joined; a member who leaves and comes back keeps that place. The member who creates a lobby is its host. When the host leaves, the member listed first becomes host, and the host can hand the role to another member. Only the host changes the settings, starts play when the lobby waits for the host, and removes members; a removed member cannot rejoin that lobby.

Every lobby has a room ID of six characters (letters and digits, without I, O, 0 and 1). Room ID lookups ignore case, spaces and dashes. A public lobby's listing carries what lobby search shows: its name, comment, room ID, host, the host's region, phase, whether it has room, its rules, and its members' names with their tags. A private lobby's listing carries only its game and build, member count, size, format and `max_games`, so a private lobby can only be found with its room ID, and the server refuses to let anyone join it without the room ID.

Joins with a different game or build identity (`game` and `build` of the server profile) are refused, when the lobby has one. A lobby ends when its last member leaves, and a lobby that nobody joins ends after 60 seconds; its room ID is released.

### Settings

The creator passes the settings as a table, and the host changes them with the `settings` command. A missing field keeps its current value (the default, when creating). Numbers out of range are clamped, an unknown `format` becomes `queue`, a `max_games` below 1 or not a whole number becomes 3, `private` is on only for `true`, and other invalid values keep the current value.

| Field | Values | Default |
|---|---|---|
| `name` | Up to 24 characters | `IKEMEN Lobby` (the lobby screens use the creator's name) |
| `comment` | Up to 40 characters | Empty |
| `size` | 2–8 members | 8 |
| `format` | `queue`, `winner_stays_on` (`winner_stays` is accepted), `round_robin`, `bracket` | `queue` |
| `max_games` | 1–99: the winner-stays limit of straight wins | 3 |
| `rounds` | 0–9 rounds to win a match; 0 uses the game options of the pairing's player 1, who hosts the netplay session | 0 |
| `time` | Round time of 10–999 seconds; 0 uses player 1's game options; -1 has no time limit | 0 |
| `teams` | `any`, `single` (single mode only) | `any` |
| `stage` | `select`, `random` | `select` |
| `start` | `auto` (play begins when players are ready), `host` (play begins when the host starts it) | `auto` |
| `interval` | 3–30 seconds between a result and the next match | 5 |
| `private` | `true`, `false` | `false` |
| `watch` | 0–10: seconds a watched match runs behind the players; 0: nobody can watch | 3 |

The host can change the settings while no match is being played or shown and no tournament is running. The size cannot drop below the member count. Changing the format starts the rotation over: the line is rebuilt in the order the members first joined, and any tournament is cleared.

`nakama.createLobby` also accepts the fields that the first version of the lobby module used (before settings and room IDs): top-level `name`, `format`, `max_games`, `max_players` (the size), and `rules.max_games` and `rules.rounds`. They fill in settings that the `settings` table does not give.

### Ready states and phases

A member is ready or not; only ready members are paired. A member stays ready after a match, so a lobby keeps rotating without anyone readying up again. A ready member who is still watching the previous match keeps the turn: a match of theirs starts when they stop watching. Three cases set members not ready: a match that broke off after the session started, for example because a player backed out at character select (both players); backing out while the connection opens (that player); and the end of a tournament (every member).

A lobby is in one of four phases:

| Phase | Meaning |
|---|---|
| `waiting` | No match is being played. The lobby waits until the next pairing's players are ready and, with `start = host`, until the host has started play. |
| `playing` | The current pairing is playing. A match that runs over 20 minutes ends without a winner (`timeout`). |
| `results` | A match has ended. The result is shown for `interval` seconds; then the next pairing starts if both of its players are ready. |
| `finished` | A round-robin or bracket tournament is over; members ready up for the next one. |

### Formats

`queue` and `winner_stays_on` are the queue formats, in which members play in turn; `round_robin` and `bracket` are the tournament formats.

- `queue`: members stand in line in join order. The first two ready members in line play; afterwards both go to the back of the line. Members who are not ready keep their place and are skipped. New members join at the back.
- `winner_stays_on`: as `queue`, but the winner stays on against the next ready member in line until reaching `max_games` straight wins, and then goes to the back too. The loser goes to the back. A winner who is not ready when the next match is due gives up the seat.
- `round_robin`: a tournament in which every entrant plays every other entrant once.
- `bracket`: a single-elimination tournament; with an odd number of entrants in a round, the last one gets a bye.

A tournament starts when every member is ready (`start = auto`, at least two members), or when the host starts it with at least two members ready (with either start setting). Its entrants are the members who are ready at that moment, in the order they first joined; members who join later wait for the next tournament. Each tournament match waits until both of its entrants are ready. An entrant who leaves forfeits: in a bracket, the opponent advances, and in round robin, the entrant's remaining matches are skipped.

A bracket is won by the last entrant standing. A round robin is won by the entrant with the most wins; when several share the most wins, it ends without a single winner and the state lists them (`tournament.tied`). The end of a tournament sets every member not ready, so the next tournament starts only when members ready up again.

Wins and losses are counted per member for as long as the lobby exists; matches without a winner do not count. Lobby results are not verified: the first report of a pairing counts. They affect only the lobby's rotation and win-loss records, not ratings.

### Matches without a winner

| Reason | When | Effect |
|---|---|---|
| `draw` | The match ended in a draw. | Queue formats: both players go to the back of the line, and a winner-stays champion loses the seat. Tournaments: the match is played again; after a second match without a winner, a bracket advances the first-listed entrant and round robin skips the match. |
| `aborted` | The session broke off after it started (a player backed out at character select, quit, or lost the connection). | As `draw`, and both players are set not ready. |
| `cancel` | A player backed out while the connection opened. | As `draw`, and that player is set not ready. |
| `p2p`, `session_failed` | The players could not connect, or the netplay session could not start (for example because the two games' content differs). | Queue formats: both players keep their place at the front of the line (a winner-stays champion keeps the seat), and the lobby does not pair these two players again. Tournaments: as `draw`. |
| `left` | A player left the lobby during the match. | Queue formats: the remaining player keeps their turn at the front of the line. Tournaments: the player who left forfeits, which counts as a win for the other player. |
| `timeout` | The match ran over 20 minutes. | As `draw`. |

### Chat

Members chat through the lobby (the `chat` command). A message has at most 80 characters, control characters are removed, and each member can send one message per half second; a faster message is refused with an error event ("Slow down"). The lobby screens draw messages, names and lobby names as typed: a `\n` typed by a player is two characters, not a line break. The lobby also sends notices when a member joins, leaves, is removed or becomes host, and when the host changes the settings or starts play. Muting is done by the client: the lobby screens hide a muted member's messages on that machine only.

### Connection quality, connection type and region

Each member's client measures its round trip to the server (`nakama.ping()`, every 3 seconds in the lobby screens) and reports it to the lobby (the `member` command) when the number of bars it shows changes, and at most every 15 seconds when it moves by 20 ms or more; the lobby shows it to every member. The client also reports its region (the server profile's `region`, cut to 8 characters; the host's region is the lobby's region in lobby search) and its connection type (below).

Two players who play each other also measure the round trip between them. While their P2P path is open, each side pings the other over it every second, and the engine reports the smoothed round trip to the lobby (the `link` command): after three answers, and then when it has moved by at least 10 ms and by a fifth, at most every 5 seconds. The lobby accepts the report only from the two players of its current match, and keeps the last value for each pair of members as long as both stay. The lobby screens show it in place of the server round trip in the slots of members this player has played, and show the current players' value in the in-game panel. The server round trip only approximates the connection between two members who have not played each other yet.

The connection type is `wired`, `wifi`, `mobile`, or empty when unknown. The engine reads it from the operating system, for the network interface that carries the connection to the server:

| System | Source |
|---|---|
| Linux, Android | `/sys/class/net/<interface>`: the wireless and phy80211 entries, the device type in `uevent`, the interface name (`wlan*`, `wlp*`, `rmnet*`, `eth*`, `enp*` and similar), then whether it is a device-backed Ethernet interface |
| Windows | The adapter's interface type (Ethernet, IEEE 802.11, mobile broadband). VPN and virtual switch adapters, which Windows lists as Ethernet, are unknown. |
| macOS | The device's hardware port (`networksetup -listallhardwareports`) |
| Other systems | The interface name |

Each player's game reports its own type; the server does not verify it. The type describes the link from the player's machine to the next device only: a machine wired to a Wi-Fi extender or a powerline adapter reports wired, a laptop on a phone's hotspot reports Wi-Fi, a phone shared over USB appears as the Ethernet adapter it presents, and a connection through a VPN is usually unknown. The `-nakama-connection wired|wifi|mobile` command-line option replaces the detected type, for testing several clients on one machine.

### A lobby match

When the lobby's current pairing (`state.current`) names this player and has a new `seq`, the lobby screens:

1. call `nakama.setPairing(peer, is_host)` (the pairing's player 1 hosts), set how the match is published with `nakama.setReplayPublish` (player 1 publishes when the lobby's `watch` setting is above 0, with that delay), and call `online.f_openSession(true, is_host)`; if that fails, they send `pair_failed` with the reason, and if the pairing ends meanwhile (the other player backed out or reported a failure), they stop waiting and return to the room;
2. run `synchronize()` and one match in game mode `netplaylobby`, with the rules from the lobby's settings (`lobby.f_setupMatch`), through `external/script/lobby_match.lua`, which adds the chosen characters to the match description (`nakama.setReplayInfo`) when the fight starts;
3. end the session with `exitNetPlay()` and `online.f_endSession()`, which closes the P2P socket but keeps the player in the lobby, and restore the default publishing;
4. report the winner with `nakama.reportLobbyResult(winner, loser, seq)`, or send `no_contest` with `draw` or `aborted`, or, when `synchronize()` failed, `pair_failed` with `session_failed`.

Both players report; the lobby applies the first report of a pairing and ignores reports for other pairings.

### Watching a lobby match

With a `watch` setting above 0, members who are not playing can watch the match being played. The pairing's player 1 publishes the match's replay stream (see [Replay streaming](#replay-streaming)) with a delay of `watch` seconds, and the lobby relays it to every member but the two players. The lobby takes stream messages only from the pairing's player 1, and drops a header over 16 KB or another message over 8 KB. With `watch` set to 0, nothing is published or relayed. The state's `current.watch` becomes true once the match's stream has started.

The lobby keeps the current match's stream messages, all streams of a Turns match included, until the next match starts: up to 2500 messages and 8 MB, enough for a 20-minute match. Past either limit, it still relays the stream, but refuses the late watchers described next. A member who did not receive the stream from its start, for example one who joined the lobby during the match, sends the `watch` command and receives the kept messages from the first header on, eight per server tick: 80 messages per second, about 40 seconds of play per second, which stays within Nakama's default outgoing queue of 64 messages per session. Messages relayed meanwhile may arrive twice; the client keeps one copy of each frame. A member who watches sends `watching` (true, and false when done), and the lobby shows the member's state as `watching`.

A spectator needs the same game content as the players, or the replay would not reproduce the match. The stream header carries the content fingerprint (see [Game compatibility](#game-compatibility)), and a spectator whose fingerprint differs cannot start watching. Lobbies already refuse members of another game or build, so the fingerprint catches differences within one game and build, such as another screenpack, roster or engine version.

The lobby screens' spectator loads the fight from the stream: the characters, palettes and team modes from the match description, and the stage from the header. It applies the players' synchronized settings and the lobby's rules, as the players' games did, and plays the match without the victory and results screens. [lobby-screenpack.md](lobby-screenpack.md) describes the screens.

### Lobby Lua API

```lua
-- Create a lobby. Emits "lobby_created" with match_id, and payload.code
-- with the room ID; the creator then joins it.
nakama.createLobby({settings = {name = "Casual Sets", size = 4, format = "queue"}})

-- List lobbies. Emits "lobby_list"; its payload is a list of matches, each
-- with label_data, the decoded listing. All lobbies are listed, private ones
-- and other games' included; the lobby screens show only public lobbies of
-- their own game and build. The optional arguments are the Nakama match
-- listing's: listLobbies([query[, label[, min_size[, max_size[, limit]]]]]),
-- by default "+label.kind:ikemen-lobby", "", 1, 8, 100.
nakama.listLobbies()

-- Resolve a room ID (emits "lobby_found" with match_id), then join with it.
nakama.findLobby("K7M2QX")
nakama.joinMatch(match_id, nil, {code = "K7M2QX"})

-- The joined lobby's latest state and its version. Given the version of the
-- previous call, it returns nil while nothing has changed.
local state, version = nakama.lobbyState(last_version)

-- Chat messages, notices, errors and removal since the last call, oldest first.
for _, ev in ipairs(nakama.lobbyEvents()) do end

-- Commands.
nakama.lobbySend("ready", {ready = true})
nakama.lobbySend("chat", {text = "gg"})

-- The winner of the pairing numbered seq (state.current.seq).
nakama.reportLobbyResult(winner_user_id, loser_user_id, seq)

-- Measure the server round trip; nakama.status().rtt_ms has the result.
nakama.ping()
```

The lobby screens handle `lobby_list`, `lobby_created` and `lobby_found` with handlers named `lobby`, so another script's handlers for these events (unnamed, or under another name) do not replace them.

Lobby state:

| Field | Content |
|---|---|
| `phase`, `phase_remaining` | The phase, and the seconds left in the results phase. |
| `code`, `host` | Room ID and the host's user id. |
| `started` | Queue formats: play has started. Tournament formats: a tournament is running or over. |
| `settings` | The settings table. |
| `members` | In join order: `user_id`, `username`, `name` (what lobbies show: the online name, or the username until one is chosen), `display_name` (the online name, empty when none), `ready`, `state` (`standby`, `ready`, `next`, `playing`, `watching`), `ping` (milliseconds, -1 unknown), `region`, `connection` (`wired`, `wifi`, `mobile`, or empty when unknown), `links` (the members with whom this member has a measured direct round trip: user id → milliseconds), `watching`, `wins`, `losses`, `host`. |
| `current` | The pairing being played: `player1`, `player2`, `player1_name`, `player2_name`, `seq`, `elapsed` (seconds), `link` (the players' direct round trip in milliseconds, once measured), `watch` (the match's stream has started). |
| `next` | The next pairing, when known: `player1`, `player2`, `player1_name`, `player2_name`. |
| `queue` | Queue formats: the line, as user ids. |
| `streak` | Winner stays: the champion's `user_id` and straight wins (`count`). |
| `last_result` | `seq`. For a win: `winner`, `loser`, `winner_name`, `loser_name`, `streak`, and `forfeit` when a tournament entrant left. For a match without a winner: `no_contest`, `reason`, `player1`, `player2`, `player1_name`, `player2_name`. |
| `tournament` | Tournament formats: `started`, `finished`, `round`, `match_index`, `winner`, `winner_name`, `tied` and `tied_names` (a round robin without a single winner), `entrants`. |

`player_count`, `players`, `format`, `max_games`, `seq` and `current_match` are also sent, for clients written for the lobby module's first version.

Lobby events (`nakama.lobbyEvents()`):

| `kind` | Fields |
|---|---|
| `chat` | `user_id`, `username`, `name`, `text` |
| `notice` | `notice` (`joined`, `left`, `host`, `kicked`, `settings`, `start`), `user_id`, `username`, `name` |
| `error` | `error`: why a command or result was refused |
| `kicked` | This client was removed from the lobby. |

Commands (`nakama.lobbySend(kind, fields)`):

| `kind` | Fields | Sent by |
|---|---|---|
| `ready` | `ready` | Any member |
| `member` | `ping`, `region`, `connection` | Any member |
| `link` | `peer`, `rtt` (milliseconds) | The engine of a player of the current match, while its P2P path to `peer`, the other player, is open |
| `watching` | `watching` | Any member |
| `watch` | `seq` (optional: the pairing to watch) | A member who is not playing |
| `chat` | `text` | Any member |
| `settings` | `settings` | Host |
| `start` | | Host |
| `kick` | `user_id` | Host |
| `host` | `user_id` | Host |
| `pair_failed` | `seq`, `reason` (`p2p`, `session_failed`, `cancel`) | The pairing's players |
| `no_contest` | `seq`, `reason` (`draw`, `aborted`) | The pairing's players |

## Server modules

The runtime entrypoint is `nakama/modules/main.lua`. Install all nine files into Nakama's runtime module path:

- `main.lua` — loads the supporting modules. Lua match handlers are resolved by module name through `nk.match_create("ikemen_session", ...)`; the Lua runtime has no `register_match`.
- `ikemen_matchmaker.lua` — uses Nakama's own matchmaker. Buckets come from the client's `+properties.*` query clauses; a `MatchmakerAdd` realtime before-hook replaces the ranked ticket's Elo with the stored rating (read by `ikemen_rating.lua`, with its settings) and appends the Elo window to the query. It creates an `ikemen_session` match for each pair, and records a ranked session's first set (see [Ranked sets](#ranked-sets)).
- `ikemen_session.lua` — Nakama coordination match for matched players, signaling, and replay chunks; it does not simulate the fight. In a ranked session it records the next sets, notes a player who leaves before a set is decided, and keeps both players' replay streams, which it stores for moderators.
- `ikemen_lobby.lua` — authoritative lobbies of up to 8 members (see [Lobbies](#lobbies)): membership, settings, ready states, pairing and tournament scheduling, results, chat, room IDs, connection types and direct round trips, and the relay of the match being played to its spectators, with the catch-up for late ones; it delivers P2P signals only to the member they name. Its RPCs are `ikemen_lobby_create` (`{settings, game, build}` → `{match_id, code}`) and `ikemen_lobby_find` (`{code}` → `{match_id, code}`). Room IDs are stored as system-owned objects in the `ikemen_lobby_codes` collection and deleted when the lobby ends.
- `ikemen_rating.lua` — per-game Elo storage, and the `ikemen_submit_result` RPC, which takes the players' claims of a ranked set's winner and settles the set once (see [Claims and ratings](#claims-and-ratings)), then writes the players' leaderboard entries and sends their notifications; `ikemen_get_rating` (`{game}` → `{rating, games, wins, losses, level, level_text, label}`) reads the caller's rating, and `ikemen_leaderboard` a game's leaderboard (see [Levels](#levels)).
- `ikemen_levels.lua` — the levels players see for their ratings, from `IKEMEN_LEVELS` (see [Levels](#levels)), used by the rating module; it registers nothing itself, and logs the levels in effect when the server starts.
- `ikemen_ranked.lua` — the ranked sets' records and their replays, shared by the modules above and the next one; it registers nothing itself.
- `ikemen_moderation.lua` — the moderator RPCs `ikemen_ranked_list`, `ikemen_ranked_replay` and `ikemen_ranked_settle`, called with the runtime HTTP key (see [Moderator RPCs](#moderator-rpcs)).
- `ikemen_account.lua` — the `ikemen_account_name` RPC (`{name}` → `{display_name}`), which sets the player's online name, a before-hook that applies the same rules to Nakama's account update, and before-hooks that keep a username Nakama would refuse from failing a sign-in (see [Online names](#online-names)).

The individual Lua modules explicitly import Nakama's `nakama` runtime module; they do not rely on an implicit global.

The matchmaker reads Elo from server-side storage. The client-supplied `elo` field is metadata only and is not used as the trusted rating source.

Ratings change only when both players of a recorded ranked set claim the same winner, or a moderator settles the set. It is not an anti-cheat system. Settlement writes a system-owned `ikemen_settlement/<set ID>` marker with version `"*"` (insert-if-absent) in the same storage transaction as both rating updates and the set's record, so retries and simultaneous claims settle a set once.

Nakama lets every signed-in player write and delete storage objects of their own, in any collection, through its storage API. The ranked records are system-owned and the modules read only those, so a player's own objects in their collections change nothing. A player's rating, though, is the player's own object (which only the server writes): on a server that does not refuse players' storage writes, a player who has no rating yet for a game could write one, which the game's leaderboard would then show. The leaderboards themselves are authoritative: Nakama refuses players' writes to them. Nakama Matchmaker's community module refuses players' writes and deletes in every `ikemen_` collection; a server that runs these modules without it needs such a before-hook on `WriteStorageObjects` and `DeleteStorageObjects` (Nakama keeps one before-hook per API, so only one module can register each).

### Required Nakama settings

| Setting | Value | Why |
|---|---|---|
| `socket.max_message_size_bytes` | `65536` | The replay stream header is ~6 KB on the wire; Nakama's 4096-byte default disconnects the sender. |
| `matchmaker.rev_precision` | `true` (recommended) | Enforces both tickets' Elo windows instead of only the searching ticket's. |
| `runtime.http_key` | A secret of the server's own | The moderator RPCs accept calls made with it (see [Moderator RPCs](#moderator-rpcs)); Nakama's default, `defaulthttpkey`, is public. |

The ranked sets' time limits and replay retention are runtime environment values (see [Ranked settings](#ranked-settings)), and so are the levels (see [Level settings](#level-settings)).

The client refreshes its session token before HTTP calls, so the 60-second default `session.token_expiry_sec` is supported. The session and lobby handlers end when their last member leaves, and after 60 seconds when nobody joins them.

## Game compatibility

A netplay session runs both games with the host's sync settings: the strict settings must already be the same in both games (except the Netcode option in a session through Nakama, which always uses rollback netcode, see [Netcode](#netcode)), and the host's other settings apply to the other player for the session. The common files and code are the exception: the `[Common]` section of the configuration (common states, commands, animations, constants and FightFX, Lua modules, and the Lua code the game runs every frame) is never taken from another game or from a replay file. Both games must list the same ones, or the session does not start; a replay whose common files and code differ does not play.

The synchronized replay header carries IKEMEN's sync settings and a SHA-256 content fingerprint. The fingerprint covers the engine version, the active motif and fight-screen definitions, `select.def` (the roster and stage list) and the common FightFX: those the fight screen loads and those the configuration lists (`Common.Fx`), which a client loads with its first match. It hashes file contents rather than absolute filesystem paths, and does not depend on what a client has played before, so a player who has played a match and one who has not agree.

The fingerprint is computed at the first `synchronize()`, before character select, so it does not cover the characters and stages of the match itself. Ranked deployment should treat it as one compatibility layer alongside Nakama game/build identity. The files of the match itself are listed in its match replay (see [What a file holds](#what-a-file-holds)), and a ranked set's replays on the server list them too, but nothing compares them before a ranked match, and the server does not verify results by replaying them.

The replay stream header carries the players' fingerprint, and a spectator's game compares it with its own before it plays the stream (see [Watching a lobby match](#watching-a-lobby-match)).

## Current implementation boundary

Implemented in this tree:

- Nakama REST device authentication, and online names chosen in the game and shown on the fight screen.
- Nakama realtime WebSocket connection and event dispatch.
- Ranked/unranked matchmaker tickets.
- Automatic join of the Nakama coordination match after matchmaking.
- Ranked sets: the server records each set of a ranked session, accepts one claim of its winner from each of its two players, changes the ratings once per set when the claims agree, and keeps both players' replays of the set for its moderators, who list the sets, export and watch the replays, and settle or void sets (see [Ranked sets](#ranked-sets)).
- Levels for the ratings, set per server and per game (`IKEMEN_LEVELS`), a leaderboard per game, and a notification of each ranked set's result, which the game shows while it searches for a ranked match and after each set (see [Levels](#levels)).
- The screen after a ranked set, which waits for the set's result in both games and ends on the same frame in both (the ready bit), and claims sent again until their answer is final, kept in `save/ranked_pending.json` across restarts (see [The screen after a ranked set](#the-screen-after-a-ranked-set) and [Claims sent again](#claims-sent-again)).
- Online play through Nakama with rollback netcode always (see [Netcode](#netcode)).
- Authoritative lobby creation/listing and up-to-8 lobby state.
- Lobby ready states, queue and winner-stays rotation, round-robin and bracket tournaments, host settings and actions, room IDs and private lobbies, and lobby chat.
- Lobby screens (lobby search, lobby settings and the lobby room, with states before, during and between matches) configured by the screenpack's `[Lobby Info]` and `[LobbyBgDef]`, and lobby matches played over the P2P netplay session.
- Lobby game/build compatibility checks (for lobbies created with a game and build).
- Watching lobby matches from the players' replay stream, from the start or late (the lobby keeps the match's stream), Turns matches included; spectators with different content are refused.
- Connection types (wired, Wi-Fi, mobile) and round trips measured over the players' direct connection, shown in lobbies.
- Nakama chat.
- Rollback replay publication with a configurable delay and rollback invalidation.
- Match replays: one file per match, for online versus and co-op matches, local versus matches (an option) and matches against the CPU in the modes a game lists, with the match's description, the players' accounts, the rules and state it started with and its inputs, played from the replay menu without the menus that led to the fight.
- Client-side replay chunk buffering per stream, including out-of-order chunk acceptance with contiguous-frame advancement.
- STUN Binding Request parsing, host/server-reflexive candidate gathering, and UDP hole punching.
- Matchmaker pairing into an IKEMEN netplay session over the P2P socket (KCP stream for the session, a GGPO channel per match).
- Lua/main-thread event marshalling.
- Explicit Nakama Lua server module entrypoint and match-handler registration.
- Motif-backed spectator loading UI with a fallback to the existing title loading motif.

Still requiring completion or validation:

- TURN/relay fallback for networks where direct UDP traversal fails.
- Watching matchmaker (quick and ranked) matches as they are played: a quick match's stream reaches only the coordination match's members, the two players, and a ranked session's streams are kept for moderators; nothing loads the fight for anyone else.
- Automatic verification of ranked results by replaying a set's replay on the server (moderators watch the replays instead; settlement itself is atomic).
- Online co-op runs: a co-op fight can fail to start with a netplay "Synchronization error" (the two games' menu lockstep frame counts differ when the fight starts), which ends the session. It has been seen at the fight after a co-op match and at a run's first fight, and predates match replays. The fights played before it keep their match replays.
- Meaningful `currentContentFingerprint()` coverage for production compatibility gating.
- Production Nakama deployment configuration and two-peer testing across real internet NATs (tested so far in a Linux network-namespace NAT lab).

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
      "stun_servers": ["stun.l.google.com:19302", "stun.cloudflare.com:3478"]
    }
  ]
}
```

This file is a client connection profile, not a secret. Replace it per distributed game build so ordinary players do not need to configure their Nakama endpoint manually.

A profile may also set `username` and `device_id`. The client signs in with a device ID, kept in `save/nakama_device_id` unless the profile gives one. `username` names the account when the device's account is first created; when Nakama would refuse it (taken, over 128 bytes, or with control characters or `[]`), the server generates one, and an existing account keeps its name. Players see a username only until they choose an online name (see [Online names](#online-names)). A shipped profile, which every player shares, normally leaves `username` out. Separate profile files with their own `username` and `device_id`, loaded with `-nakama-config <file>`, are useful for testing several clients on one machine.

## Optional dedicated-server launcher

Dedicated servers are optional. A normal player does not need a local Nakama process when the game ships with a remote profile. A community that wants to operate its own deployment can use IKEMEN's headless command-line server tools:

```text
Ikemen_GO -server-wizard
Ikemen_GO -server -server-config save/server.json
```

`-server-wizard` is intentionally headless: it prompts in the terminal and writes an IKEMEN server-manager JSON profile. The wizard also writes a companion `save/server-client.json` profile that can be copied into `data/online/servers.json` after the public hostname, key, and TLS settings are verified.

`-server` launches the configured Nakama executable as a child process, forwards its stdout/stderr, and can append to a configured log file. IKEMEN is the launcher/manager; it does not embed the Nakama server or its database. The community server still supplies the Nakama binary, Nakama runtime configuration, and required database deployment.

The dedicated server configuration includes the intended matchmaking, lobby, ranked, spectator, chat, queue/winner-stays defaults and port assignments so the community deployment is documented in one place.
