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

-- Default lobby behavior is FIFO queue rotation.
nakama.createLobby({
    name = "Open Lobby",
    format = "queue",
    max_players = 8
})

-- Optional winner-keeps-playing lobby. The winner may remain active for
-- at most three consecutive games before returning to the end of the queue.
nakama.createLobby({
    name = "Winner Stays - 3 Game Cap",
    format = "winner_stays_on",
    max_players = 8,
    max_games = 3
})
