
<p align="center">
  <a href="https://ikemen-engine.github.io/">
    <img src="https://github.com/user-attachments/assets/0dcd7ae1-5c9d-44e1-aa32-9ec4b9ed3952" style="width: 699px; alt="IKEMEN GO Logo"/>
  </a>
</p>

Ikemen GO is an open source fighting game engine that supports resources from the [M.U.G.E.N](https://en.wikipedia.org/wiki/Mugen_(game_engine)) engine, written in Google’s programming language, [Go](https://go.dev/). It is a complete rewrite of a prior engine known simply as Ikemen.

## About this fork
This repository is a fork of Ikemen GO that adds online play through [Nakama](https://heroiclabs.com/nakama/), an open-source game server. Players sign in to a Nakama server and find opponents there; the two players of a match then connect directly to each other and play over that connection. The server handles accounts, matchmaking, lobbies, chat and ratings; it does not run the fights. The rest of this README is upstream's text, apart from one line under [Installing](#installing).

What the fork adds:

- **Online play.** Quick (unranked) and ranked matches found by the server's matchmaker, and lobbies where up to eight players chat and play one-on-one matches in turn, or in round-robin and bracket tournaments. Each player chooses an online name, which lobbies show, and which the fight screen of an online match shows in place of the character's name. Online play through Nakama always uses rollback netcode: each game runs on at once with a guess of the other player's input, and corrects the frames it guessed wrong when the input arrives. Host Game and Join Game are unchanged.
- **Ranked sets.** Ranked players play best-of-N sets (three matches by default), and each set changes their Elo ratings once. The server records every set and accepts a result only from that set's two players. When both games claim the same winner, the ratings change; when they disagree, or only one claim comes in time, the set is disputed and the server's moderators (whoever runs it) decide it. They can watch both players' replays of the set, which the server keeps (30 days by default). The server does not replay matches to check them itself.
- **Levels and leaderboards.** Players see a level computed from their rating, on a scale that each server's creator sets: for example, Power level 9,001 for a new player and billions for the best. Each game has a leaderboard on the server.
- **Ranked results.** After a ranked set, both games wait together, up to 10 seconds, for the server's result, then show who won and the player's level before and after. A game without a final answer keeps sending its claim, after a restart too, until the answer is final or the claim is a day old.
- **Watching and replays.** Lobby members who are not playing can watch the current match a few seconds behind the players, when the lobby allows it. Matches played online with rollback netcode (every match through Nakama) are saved as replay files in `save/replays`, and local versus matches too when an option is on; WATCH MODE > REPLAY plays them.

Online play needs a Nakama server running this fork's server modules (`nakama/modules`), and a *server profile*, `data/online/servers.json`, that names the server; the profile in this repository is an example for a game to replace. The packages include the modules, an example server configuration and the documentation in `nakama/`. `Ikemen_GO -server-wizard` writes the settings for a community's own server, and `Ikemen_GO -server` starts Nakama with them; Nakama itself and its database are installed separately. A screenpack adds the online menu items (QUICK MATCH (ONLINE), RANKED MATCH (ONLINE) and ONLINE LOBBIES); the default menus do not have them. There is no relay yet for networks where the direct connection fails, and play across real internet NATs has not been tested yet.

[Nakama Matchmaker](https://github.com/RollinJay/Nakama-Matchmaker) is a launcher for this fork and for the [mGBA](https://github.com/RollinJay/mgba) and [melonDS](https://github.com/RollinJay/melonDS-test) forks, which also play online through Nakama. For an Ikemen GO game, its **Play online** starts this fork signed in as the launcher's player, and its **Leaderboard** shows the game's leaderboard. It can also run a community server, set up with this fork's server modules ([docs/hosting.md](https://github.com/RollinJay/Nakama-Matchmaker/blob/main/docs/hosting.md)); a server with these modules also serves the launcher and the mGBA fork.

- **Downloads:** the [nightly](https://github.com/RollinJay/IkemenGo-Nakama/releases/tag/nightly) pre-release, rebuilt at 8 PM Pacific time on each night after new commits to `main`.
- **Details:** [nakama/README.md](nakama/README.md) describes the online features, the server modules and their settings, and the menu items; [nakama/lobby-screenpack.md](nakama/lobby-screenpack.md) describes the lobby screens and how a screenpack styles them.
- **License:** unchanged, MIT (see [License](#license)).

## Features
Ikemen GO aims for backwards-compatibility on par with M.U.G.E.N version 1.1 Beta, while simultaneously expanding on its features in a variety of ways.

This does not mean bug-for-bug emulation. Behavior may intentionally differ when matching M.U.G.E.N would mean preserving bugs, quirks, or unnecessarily limiting the engine.

Refer to [our wiki](https://github.com/ikemen-engine/Ikemen-GO/wiki) to see a comprehensive list of new features that have been added in Ikemen GO.

## Installing
Ready-to-use builds are available in the [releases section](https://github.com/ikemen-engine/Ikemen-GO/releases). Stable releases use tags such as `v1.0.0`, while release candidates use tags such as `v1.0.0-rc.1` and are marked as pre-releases. [Nightly builds](https://github.com/ikemen-engine/Ikemen-GO/releases/tag/nightly) are updated after each commit to `develop` and may be less stable.

This fork's own builds are on its [releases page](https://github.com/RollinJay/IkemenGo-Nakama/releases): the [nightly](https://github.com/RollinJay/IkemenGo-Nakama/releases/tag/nightly) pre-release, rebuilt at 8 PM Pacific time on each night after new commits to `main`, for Windows, Linux, macOS and Android. The links above lead to upstream's builds, which do not have the fork's online play.

### System requirements
| Platform | Minimum |
|---|---|
| Windows | Windows 10 (64-bit) |
| macOS | macOS 13 Ventura (Apple Silicon) |
| Linux | x86-64 with glibc 2.35+, e.g. Ubuntu 22.04, Debian 12, Fedora 36 |
| Android | Android 14 (API 34, arm64) |

A GPU supporting OpenGL 3.3, OpenGL ES 3.2 or Vulkan is required.

> Windows 7/8 and 32-bit Windows are no longer supported; use an older release if
> you need them. Intel macOS builds are not currently published.

## Running
Download the ZIP archive that matches your operating system and extract its contents to your preferred location.

On Windows, double-click `Ikemen_GO.exe`.
On macOS or Linux, double-click `Ikemen_GO.command`.

## Developing
These instructions are for those interested in developing the Ikemen GO engine itself. Instructions for creating custom stages, fonts, characters and other resources can be found in the community forum.

### Building
For setup and platform-specific steps, see [BUILDING.md](./BUILDING.md).
It covers Windows, Linux, macOS, and Android.

### Debugging
In order to run the compiled Ikemen GO executable, you will need to download the [engine dependencies](https://github.com/ikemen-engine/Ikemen-GO-Screenpack) and unpack them into the Ikemen-GO source directory. After that, you can use [Goland](https://www.jetbrains.com/go/) or [Visual Studio Code](https://code.visualstudio.com/) to debug.

## Troubleshooting
If you run into any issues with Ikemen Go, you can report it on our [issue tracker](https://github.com/ikemen-engine/Ikemen-GO/issues). It is recommend to read [this page](https://github.com/ikemen-engine/Ikemen-GO/blob/develop/CONTRIBUTING.md) before submitting a bug report.

## References
- [The original reposity of Ikemen GO.](https://osdn.net/users/supersuehiro/pf/ikemen_go/) This project was forked from this repository due to its original author seemingly abandoning the project.

- [The default motif bundled with the engine.](https://github.com/ikemen-engine/Ikemen-GO-Screenpack) Note that this motif is licensed under CC-BY 3 rather than Ikemen GO's source, which is MIT.

## Name
"Ikemen" is an acronym of:

**い**つまでも **完**成しない **永**遠に **未**完成 **エン**ジン  
**I**tsu made mo **K**ansei shinai **E**ien ni **M**ikansei **EN**gine

## License
Ikemen GO engine is under the MIT License.
Bundled screenpack assets are under Creative Commons licenses.
See [LICENCE.txt](LICENCE.txt) for more details.
This program dynamically links FFmpeg (LGPL v2.1).

The exact corresponding source for the FFmpeg build is provided on the [release page](https://github.com/ikemen-engine/Ikemen-GO/releases/latest) as `src_ffmpeg.tar.gz`. You may rebuild this application against a modified FFmpeg.
