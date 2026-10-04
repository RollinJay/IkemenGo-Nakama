-- Minimal game-side example.
-- The actual screenpack/game should decide how to present these states.

nakama.connect({
    host = "127.0.0.1",
    port = 7350,
    server_key = "defaultkey",
    game = "my_game",
    build = "1.0.0",
    region = "us-west"
})

nakama.on("connected", function(_, event)
    -- Show online UI.
end)

nakama.on("matchmaker_ticket", function(_, event)
    -- Matchmaking is active: event.ticket
end)

nakama.on("matchmaker_matched", function(_, event)
    -- The engine automatically joins event.match_id.
end)

nakama.on("match_joined", function(_, event)
    -- At this point the control-plane match exists.
    -- The final GGPO transport attachment will use the NAT-traversed socket.
end)

nakama.on("replay_header", function(_, event)
    -- event.payload contains the deterministic replay header.
end)

nakama.on("replay_chunk", function(_, event)
    -- Chunks are already retained in the engine-side replay buffer.
end)

nakama.on("p2p_connected", function(_, event)
    -- Direct UDP path is established.
end)

nakama.on("chat_message", function(_, event)
    -- event.chat contains the Nakama channel message.
end)

-- Lobbies (see README.md, Lobbies). Lobby requests need the connection, so
-- the example lobby is created once "connected" has arrived. The lobby
-- screens (external/script/lobby.lua) create, list and join lobbies the same
-- way. These handlers are named ("example"), so they do not replace another
-- script's handlers for the same events.
nakama.on("connected", function(_, event)
    -- The default format is FIFO queue rotation. A winner-keeps-playing lobby
    -- whose winner stays for at most three games in a row would use
    -- settings = {name = "Winner Stays - 3 Game Cap", format = "winner_stays_on", max_games = 3}.
    nakama.createLobby({
        settings = {
            name = "Open Lobby",
            size = 8,
            format = "queue"
        }
    })
end, "example")

nakama.on("lobby_created", function(_, event)
    -- event.match_id, and event.payload.code: the room ID. A client is in one
    -- match at a time: leave the current one (nakama.leaveMatch()) first.
    nakama.joinMatch(event.match_id, nil, {code = event.payload.code})
end, "example")
