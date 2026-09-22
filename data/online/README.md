# Nakama Server Profiles

`servers.json` is the client-side connection profile shipped with the game. Replace it per game build so the default server, game identifier, build identity, TLS settings, region, and STUN list point at the intended deployment.

Normal players do not need to run a server locally.

A dedicated community host can run `Ikemen_GO -server-wizard` to create a server-manager profile and a companion client profile. The launcher starts an external Nakama executable; Nakama still supplies its own runtime configuration and database deployment.
