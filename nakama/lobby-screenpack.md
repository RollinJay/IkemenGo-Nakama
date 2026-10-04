# Online lobby screens

An online lobby is a room on the online server where up to eight players gather to play one-on-one netplay matches in turn. Players find a lobby in a list of public lobbies, or join a private one with its room ID. In the lobby room, each member marks themselves ready; the lobby pairs ready members, the two paired players play their match over a direct connection between their machines, and the other members see who is playing, can watch the match a few seconds behind the players, and then see the result, before the next pairing starts. Players appear in lobbies under the online name they choose.

The lobby has three screens:

- **Lobby search**: the list of public lobbies, the details of the highlighted one, this player's online name, and actions to join, refresh, create a lobby, join by room ID, filter the list and change the name.
- **Lobby settings**: the lobby's name, size, format, match rules and spectators; used to create a lobby, and by the lobby's host to change it.
- **Lobby room**: the members with their state, connection type (wired, Wi-Fi or mobile), connection quality, region (set in each player's server profile) and record; the room menu; what the lobby is doing (waiting for players, a match being played, the last result); and the chat. Members who are not playing can watch the match being played from here.

`external/script/lobby.lua` draws these screens, and the online name entry, from the screenpack: texts, fonts, sprites, boxes, menus and positions from `[Lobby Info]`, the background from `[LobbyBgDef]`, the music from `[Music]`, and message boxes from `[Warning Info]` as on other screens. The engine's built-in defaults (`src/resources/defaultMotif.ini`) define a complete 320×240 `[Lobby Info]` layout, so the lobby works with a screenpack that sets none of these keys; a screenpack that restyles the lobby sets only the keys it changes. Two kinds of text are not screenpack keys and appear as they are, in English: messages from the server (a refused join such as "Lobby is full", or "Slow down" in chat) and the connection failure messages of the online scripts.

How the server runs a lobby (who plays whom, formats, tournaments, results without a winner, host actions, room IDs, watching) is described in [README.md, Lobbies](README.md#lobbies), and online names in [README.md, Online names](README.md#online-names). This document describes what the screens show and how a screenpack configures them.

## Adding the lobby to a screenpack

The lobby is opened from a title menu item named `onlinelobby`. The default title menu does not include it; a screenpack adds it where it wants it, for example in its network submenu:

```ini
[Title Info]
menu.itemname.menunetwork.onlinelobby = ONLINE LOBBIES
```

The game also needs a server profile (`data/online/servers.json`, see [README.md, Shipped client server profiles](README.md#shipped-client-server-profiles)), and fonts that can show every online name.

### Fonts

Online names are chosen from the characters the lifebar's name fonts can draw, because the fight screen shows each player's name in place of a character's name (see [README.md, Online names](README.md#online-names)). The lobby shows the same names, so the fonts of these texts need a glyph for each character the lifebar's name fonts have: `status` (the opponent while a match opens, the players while a watched match loads), `textinput` (the name entry), `browser.account`, `browser.detail.host`, `browser.detail.member`, `room.slot.name`, `room.status`, `room.match.p1`, `room.match.p2`, `room.results.winner`, `room.results.loser`, `room.results.reason`, `room.results.next`, `room.chat.name` and `room.chat.notice`. Several of them also show members' tags, and `room.slot.tag` shows only tags: a tag is four digits, which the default texts put in parentheses. In screenpack files, `;` and `#` start a comment even inside double quotes, so a text that needs either character is written between backquotes (`` `%s #%s` ``). A `\n` in a screenpack text starts a new line; one that a player types in a name, lobby name, comment or chat message does not.

The default layout uses `f-6x9` and `f-4x6` for these texts. The `f-6x9` of IKEMEN GO's default screenpack draws `_` as a solid block, as it does `#`, `$`, `&`, `*`, `@`, `^`, `` ` ``, `|` and `~`, so a game whose names can have those characters needs a copy of the font with real glyphs. The engine looks for a font named in `[Lobby Info]` in the screenpack's folder (and its `font/` subfolder) before the game's `font/` folder, so a screenpack can ship its own copy of either font.

Lobby names, comments and chat can contain any character a player types. Bitmap fonts draw printable ASCII only; other characters need a TrueType font, shipped with the game rather than taken from the system. The chat's texts (`room.chat.name`, `room.chat.text`, `room.chat.notice`, `room.chat.error` and `room.chat.input`) use M PLUS 1 by default: `font/mplus1.def`, with the font file and its license (SIL Open Font License) in `font/MPLUS1`. It covers Latin, Vietnamese included, Japanese kana and 5,289 kanji. It has no Greek, Cyrillic or Hangul, no Chinese characters beyond those kanji, no right-to-left scripts and no emoji, and few symbols: no arrows, and none of ★ ♥ ♪ ○ ● □ ※ ✓ or ℃; M+ ships other weights and families (M PLUS 2, M PLUS U) with the same coverage, and only their `.ttf` files load. The engine's TrueType renderer decides which fonts work:

- It reads TrueType outlines only. A font with CFF outlines (most `.otf` files, including Noto Sans CJK) or a color emoji font (such as Noto Color Emoji) stops the game when it loads.
- It finds characters outside Unicode's Basic Multilingual Plane, such as most emoji, only when the first Unicode-platform subtable of the font's character map covers them. In most fonts that subtable covers the Basic Multilingual Plane only, so their emoji are drawn as missing characters; removing the platform 0 subtables (with fontTools, for example) makes the engine use the Windows full-repertoire subtable instead. Private-use characters (U+E000 to U+F8FF), which a game can fill with its own icons, are in the Basic Multilingual Plane.
- Each text has one font, with no fallback: a character the font lacks is drawn as the font's missing-character glyph, which in M PLUS 1 is an empty box.
- It draws characters one at a time, without shaping. Latin, Greek, Cyrillic, Chinese, Japanese and Korean come out right; scripts written right to left or with joined or reordered letters (Arabic, Hebrew, Devanagari, Thai) come out wrong.

## The screens

### Lobby search

Lobby search lists the public lobbies of the same game and build (the `game` and `build` of the server profile): lobbies with room first, then lobbies waiting for their next match (not playing, showing a result or finished), then by number of members, most first, and by name. Each row shows the lobby's name, comment, members and size, and its phase (open, in a match, finished, or full). A details panel shows the highlighted lobby's room ID, host, rules and member names. The player's own online name is shown at the top (`browser.account`).

The list keys (`browser.list.next.key`, `browser.list.previous.key`) move through the list; the menu keys move along the action menu:

| Item | Action |
|---|---|
| `join` | Joins the highlighted lobby and opens its room. |
| `search` | Refreshes the list. |
| `create` | Opens lobby settings. Confirming creates the lobby and opens its room with this player as host. |
| `code` | Asks for a room ID and joins that lobby. Private lobbies can only be joined this way. |
| `filter` | Cycles which lobbies are listed: all, open only (not full), my region (open lobbies whose host is in this player's region). The item shows the current filter. |
| `account` | Asks for a new online name (see [Online name](#online-name)). |
| `back` | Returns to the title menu. |

Private lobbies are never listed. Leaving a room returns to lobby search, which refreshes itself.

### Online name

Other players see each player under their online name (the account's display name, see [README.md, Online names](README.md#online-names)), and the fight screen shows it in place of the name of the character the player controls. A name has 2 to 16 characters, each one the lifebar's name fonts can draw. Names need not be unique: the lobby screens show each member's tag beside the name (for example `KAI (7841)`), four digits taken from the account ID that tell members with the same name apart. A name this game's lifebar cannot draw completely, chosen in a game with another lifebar, shows the character's name on the fight screen.

The first time a player opens lobby search with an account in a session, the game checks whether the account has an online name. If not, text entry asks for one (`textinput.text.firstaccount`); cancelling keeps the account without a name (lobbies show its username meanwhile), and the question comes back in the next session. The `account` item of lobby search changes the name later (`textinput.text.account`, starting from the current name).

A name with characters the lifebar cannot draw is refused with `message.namefont`, which lists them (characters outside ASCII as code points, such as U+00E9, since the message's font may lack them too); a name that is too short, or that the server refuses, shows `message.nameinvalid`. The entry then opens again with the name as typed. While the name is saved, `status.text.account` is shown; a failure on the server shows `message.namefailed` and keeps the old name. When the server does not answer in time (`lobby.accountFrames`, 35 seconds), the same message shows, although the server may still save the name. A new name shows in the next lobby joined.

### Lobby settings

Lobby settings creates a lobby (title `settings.title.text.create`) or, for the host, changes the current one (title `settings.title.text.edit`; the confirm item then shows `valuename.apply`). Up and down move through the items, left and right change the highlighted value, and confirm on the name or comment opens text entry. A line at the bottom describes the highlighted item (`settings.info.text.<item>`).

| Item | Setting | Values offered |
|---|---|---|
| `name` | Lobby name | Text, up to 24 characters. Empty uses the player's name. |
| `comment` | Comment shown under the name | Text, up to 40 characters. |
| `size` | Most members | 2 to 8 |
| `format` | How pairings are made | Queue and winner stays (the queue formats: members play in turn); round robin and bracket (the tournament formats) |
| `maxgames` | Straight wins before a winner leaves the seat; shown for winner stays only | 1, 2, 3, 4, 5, 7, 10, 15, 20, 99 |
| `rounds` | Rounds to win a match | Default, 1 to 5 |
| `time` | Round time in seconds | Default, 30, 45, 60, 90, 99, no limit |
| `teams` | Team modes allowed | Any, single only |
| `stage` | Stage choice | Select, random |
| `start` | When play begins | Auto, host |
| `interval` | Seconds the result is shown between matches | 3, 5, 8, 10, 15, 20, 30 |
| `type` | Lobby type | Public, private |
| `watch` | Spectators: how far behind the players a watched match runs | Off (nobody can watch), 1, 3, 5, 10 seconds |
| `confirm` | Creates the lobby, or applies the changes | |
| `back` | Leaves without changes | |

"Default" rounds and time use the game options of the match's player 1 (the first-listed player of the pairing, who hosts the netplay session). The value lists come from `lobby.choices` (see [Script hooks and tunables](#script-hooks-and-tunables)).

The host opens this screen from the room menu while no match is being played or shown and, in the tournament formats, while no tournament is running. The room keeps running behind it: the screen closes by itself when this player's match starts or the room closes.

### Lobby room

The room shows:

- a header with the lobby name, comment, room ID, member count and a rules summary;
- eight member slots, in the order the members first joined: filled slots for members, open slots up to the lobby size, and closed slots beyond it;
- the room menu;
- the status line, or the in-game panel while a match is played, or the results panel between matches;
- the chat log.

Each member slot shows the member's number, name, a host marker, a marker for this player's own slot, region (the `region` of the member's server profile, as text or a flag sprite), connection type, connection bars, record in this lobby (wins and losses), and a state badge:

| Badge | Member state |
|---|---|
| `standby` | Not ready. |
| `ready` | Ready, waiting for a turn. |
| `next` | Ready and in the next pairing. |
| `playing` | In the match being played. |
| `watching` | Watching the match being played. |

The connection type is how the member's machine is connected: wired, Wi-Fi or mobile data. Each member's game reads it from the operating system and reports it; nothing is shown when it is unknown (for example through a VPN). It describes only the link from the machine to the next device: a machine wired to a Wi-Fi extender shows wired.

The connection bars show the member's round trip to the online server, measured every few seconds by each member's game. For a member this player has played, they show instead the round trip measured over the two players' direct connection during their match (`room.slot.ping.directon`, `room.slot.ping.direct`), which is the one that matters when the two play each other again.

The room menu:

| Item | Action |
|---|---|
| `ready` | Marks this player ready, or not ready again; shows `room.menu.valuename.unready` while ready. A member stays ready after a match, so the lobby keeps rotating without anyone selecting READY again. |
| `chat` | Opens chat entry at the bottom of the room. |
| `settings` | Host only: lobby settings (see above for when it is offered). |
| `start` | Host only. In the queue formats with the start setting host, begins pairing; offered until the host has used it. In the tournament formats, starts a tournament with the members who are ready; offered whenever no tournament is running, also when the start setting is auto. |
| `watch` | Watches the match being played (see [Watching a match](#watching-a-match)). Offered to members who are not playing, once the match's fight has started, when the lobby's spectators setting is not off. |
| `players` | Moves a cursor over the member slots; confirm opens the player menu for that member. |
| `leave` | Leaves the lobby. Cancel first moves the cursor to this item, and leaves when pressed on it. |

The player menu (`room.playermenu`) has `mute` (hides that member's chat on this machine; shows `valuename.unmute` for a muted member), `kick` (host only: removes the member, who cannot rejoin this lobby), `host` (host only: hands the host role to the member) and `back`.

#### What the room shows before, during and between matches

| Lobby phase | The room shows |
|---|---|
| Before a match (waiting) | The status line says what the lobby waits for: `room.status.text.waithost` (the host has not started play), `notready` (this player is not ready), `waiting` (this player is ready; not enough other members are), `waitall` (a tournament that starts when everyone is ready), or `waitentrant` (a tournament match waits for the named entrant to ready up). |
| In game (playing) | The in-game panel (`room.match`): both players' names, the time since the match started, the round trip over the players' direct connection once they have measured it and, in winner stays, the champion's streak. The badges show who is playing, who is watching and, in the queue formats, who plays next. |
| Between matches (results) | The results panel (`room.results`) for the lobby's interval: the winner and loser, or "no contest" and why; the next pairing when it is known; a countdown. The status line shows `room.status.text.results` when a next pairing is known and otherwise what the lobby waits for. |
| Tournament over (finished) | The status line shows `room.status.text.finished` with the winner, or `finishedtie` with the entrants who share the most wins in a round robin. The results panel of the final match uses `room.results.title.text.finished` as its title. The end of a tournament sets every member not ready: members ready up again for the next one. |

The two players of a pairing leave the room for their match. A full-screen message shows while the direct connection opens (`status.text.pairing`) and while the session starts (`status.text.session`); Esc or the title menu's cancel key (`[Title Info]` `menu.cancel.key`) backs out, which ends the match as cancelled, returns both players to the room and marks this player not ready. If the connection cannot be opened, or the two games cannot start a session together (for example because their content differs), every member sees the match end without a winner; in the queue formats the lobby does not pair those two players again, and a tournament tries the match once more before moving on. Otherwise both players go through character select, stage select (when the lobby's stage setting is select), the versus screen, the fight and, if the screenpack enables it for versus matches, the victory screen, and then return to the room. The winner is reported to the lobby. A draw ends the match without a winner; so does an interrupted match (a player backing out at character select, quitting or losing the connection), which also marks both players not ready.

#### Watching a match

WATCH plays the match being played on this machine, from the players' own inputs, as the players see it but a few seconds behind them (the lobby's spectators setting). The full-screen message `status.text.watching` shows the two players' names while the match's first seconds arrive; Esc or the lobby search menu's cancel key (`browser.menu.cancel.key`) stops waiting. Then the fight loads with the players' characters, palettes, team modes and stage, and plays; the other members see this player as watching. A member who starts watching after the match began receives the match from its start and plays it at four times the speed until it has caught up with what has arrived. The fight ends without the victory and results screens, and the room returns. If the players' inputs stop arriving for 20 seconds plus the spectators setting (a stalled connection), watching ends the same way. A member who is ready keeps their turn while watching: when their match comes up, it starts once they are back in the room.

Watching needs the same game content as the players': the same engine version, screenpack, fight screen and roster. A member whose content or settings differ from the players' gets `message.watchcontent`; a match that cannot be watched for another reason (it ended, or its stream was withdrawn, before watching could start) gets `message.watchfailed`.

#### Chat

Chat entry opens at the bottom of the room: typing adds text, the textinput keys edit it (`textinput.truncate.keycode` deletes a character, `textinput.trim.keycode` clears the text, `textinput.paste.keycode` pastes), `textinput.confirm.keycode` sends and Esc cancels. A message has at most 80 characters, and the server accepts one message per half second from each member. The log shows the last `room.chat.lines` rows; long messages wrap inside the chat box. Besides messages, the log shows notices (a member joined, left, was removed or became host; the host changed the settings or started the lobby) and errors (for example "Slow down", or a connection that failed).

## How the lobby elements are configured

### Coordinates

All lobby positions, offsets and boxes use `[Lobby Info]` `localcoord` (320, 240 by default); an individual text or sprite may still set its own `localcoord`, as in other sections. A screenpack that restyles the lobby for another resolution sets `localcoord` and then positions every element it uses.

Things that repeat have a position and a spacing: list rows (`browser.list.pos`, `browser.list.spacing`), member slots (`room.slot.pos`, `room.slot.spacing`), chat lines (`room.chat.pos`, `room.chat.spacing`). The parts of a row, slot or line are placed with an `offset` relative to that position. Panels that show one thing (the lobby details, the in-game panel, the results panel) have a `pos`, and their parts are placed relative to it the same way. Titles, counters, the room header, status lines and `panel.<name>` boxes use screen coordinates.

### Element types

- **Text**: the usual text keys of other screens (`font`, `offset`, `text`, `scale`, `layerno`, `window`, `localcoord` and so on). The third `font` value is the alignment: 1 left, 0 centered, -1 right. A text that receives values uses `printf` patterns; the key tables below list them (`%s` a name or text, `%d` a number). A text left empty is not drawn, so a screenpack hides a text by setting its `text` to nothing (in an element, this hides only the text part); only the chat's message and error lines are drawn as they arrive.
- **Sprite or animation**: `anim` (an action of the screenpack's animations) or `spr` (group, index), with `offset` and the usual sprite keys.
- **Box**: a filled rectangle. `visible` (nothing is drawn unless 1), `coords` (x1, y1, x2, y2 relative to what owns the box), `col` (r, g, b), `alpha` (source, destination, as for other screenpack boxes such as `boxbg`), `layerno` and `localcoord`.
- **Element**: a box, a sprite or animation, and a text that share one `offset`; the box's `coords` are relative to that offset. Any of the three can be left out. The box is drawn first, then the sprite, then the text. List rows, slot backgrounds, the slot cursor, the host and self markers, badges and the backgrounds of the in-game and results panels are elements, so each can be a plain box, a sprite, a text, or a combination.
- **Menu**: the menu keys used by other screens (`pos`, `item.*`, `item.active.*`, `item.value.*`, `item.spacing`, `boxcursor.*`, `boxbg.*`, `window.visibleitems`, `arrow.*`, `next.key`, `previous.key`, `done.key`, `cancel.key`, `itemname.<item>`, `valuename.<value>`). Lobby settings also uses `add.key` and `subtract.key`. `item.spacing` sets the direction: `0, 15` stacks items vertically, `50, 0` lays them out in a row.
- **Text map**: several texts of one element, chosen by a key: `<element>.text.<key>`.
- **Fade, sound, key**: as in other sections: `fadein.*` and `fadeout.*` take the usual fade keys (`time`, `col`, `anim`, `snd`); a sound is `group, index` in the screenpack's sound file (-1 for none); menu keys are button names (`U`, `D`, `L`, `R`, `a`, `b`, `c`, `x`, `y`, `z`, `s`, `m`), and `keycode` values are keyboard key names (`RETURN`, `BACKSPACE`).

Menu items appear in the order the screenpack lists them, followed by any default items it does not list. An item whose `itemname` is set to nothing is hidden. Hiding `ready` or the settings `confirm` item removes the only way to ready up or to create a lobby; without `leave` or `back`, the cancel key still leaves the screen.

## Key reference

### General

| Key | Type | Meaning |
|---|---|---|
| `localcoord` | | Coordinate space of the lobby screens. |
| `fadein.*`, `fadeout.*` | fade | Fade in when lobby search or the room opens and after a match; fade out when lobby search closes. |
| `cursor.move.snd`, `cursor.done.snd`, `cancel.snd` | sound | Menu sounds. |
| `event.<event>.snd` | sound | `join` (a member joined), `leave` (a member left or was removed), `ready` (this player readied up), `chat` (a message from another member), `match` (this player's match is starting), `error`. |
| `status` | text | Full-screen message while waiting on the server or the other player. |
| `status.text.<key>` | text map | `connecting`, `searching`, `creating`, `finding` (looking up a room ID), `joining`, `pairing` (%s: the opponent), `session` (%s: the opponent), `account` (saving the online name), `watching` (%s, %s: the two players, while watching waits for the match). |
| `status.overlay.col`, `status.overlay.alpha` | | Overlay drawn under the message. |
| `textinput` | text | Text entry for the lobby name, comment, room ID and online name. |
| `textinput.text.<key>` | text map | Prompts: `name`, `comment`, `code`, `account` (a new online name), `firstaccount` (the online name, on the first visit). |
| `textinput.overlay.col`, `textinput.overlay.alpha` | | Overlay drawn under the text entry. |
| `textinput.confirm.keycode`, `.trim.keycode`, `.truncate.keycode`, `.paste.keycode` | key | Confirm, clear, delete a character, paste. Also used by chat. |
| `valuename.<value>` | | Display names; see [Value names](#value-names). |
| `message.<key>` | | Texts shown in a message box: `nameinvalid`, `namefont` (%s: the characters the lifebar cannot draw), `namefailed` (see [Online name](#online-name)), `watchfailed`, `watchcontent` (see [Watching a match](#watching-a-match)). |

### Value names

`valuename.<value>` names the values shown in lobby settings, lobby search and the room:

| Keys | Used for |
|---|---|
| `queue`, `winner_stays_on`, `round_robin`, `bracket` | Formats. |
| `public`, `private` | Lobby type. |
| `auto`, `host` | Start setting. |
| `any`, `single` | Teams setting. |
| `select`, `random` | Stage setting. |
| `default` | Rounds or time left to player 1's game options. |
| `infinite` | Round time without a limit. |
| `none` | An empty comment. |
| `all`, `open`, `region` | Lobby search filters. |
| `apply` | The confirm item while editing an existing lobby. |
| `off` | Spectators off. |
| `size`, `maxgames`, `rounds`, `time`, `interval`, `watch` | Setting values (%d). |
| `streak_rule` (%d), `rounds_rule` (%d), `time_rule` (%d), `infinite_rule`, `single_rule`, `random_rule`, `private_rule`, `nowatch_rule`, `separator` | Parts of the rules summary. |

The rules summary is the format's name (with `streak_rule` added for winner stays), followed by `rounds_rule` when rounds are set, `time_rule` or `infinite_rule` when the round time is set, and `single_rule`, `random_rule`, `private_rule` and `nowatch_rule` (spectators off) when those settings apply. The room header joins the parts with `separator` and a space (for example `WINNER STAYS (3), FIRST TO 2, 99 SEC`); the lobby search details put each part on its own line. `separator` and a space also join the names in `room.status.text.finishedtie`.

### Lobby search

| Key | Type | Meaning |
|---|---|---|
| `browser.panel.<name>` | box | Boxes drawn behind the screen in name order, in screen coordinates. |
| `browser.title` | text | Screen title. |
| `browser.count` | text | Number of lobbies listed (%d). |
| `browser.account` | text | This player's online name (%s). |
| `browser.menu` | menu | Items `join`, `search`, `create`, `code`, `filter`, `account`, `back`. `item.value` shows the current filter under `filter`. |
| `browser.list.pos`, `browser.list.spacing` | | First row, and the step from one row to the next. |
| `browser.list.visibleitems` | | Rows shown at once. |
| `browser.list.next.key`, `browser.list.previous.key` | | Keys that move through the list. |
| `browser.list.box` | box | Behind the whole list, relative to `browser.list.pos`. |
| `browser.list.header` | text | Column headings, in screen coordinates. Not shown by default. |
| `browser.list.row`, `browser.list.active` | element | Behind each row, and behind the highlighted row instead. |
| `browser.list.name`, `browser.list.comment` | text | Lobby name and comment (%s), cut off at the row box when it is visible. |
| `browser.list.players` | text | Members and size (%d, %d). |
| `browser.list.phase.text.<phase>` | text map | `waiting`, `playing`, `results`, `finished`; `full` replaces them while the lobby is full. |
| `browser.list.format` | text | Format name (%s). Not shown by default. |
| `browser.list.code` | text | Room ID (%s). Not shown by default. |
| `browser.list.host` | text | Host name and tag (%s, %s). Not shown by default. |
| `browser.list.empty` | text | Shown when no lobby is listed, relative to `browser.list.pos`. |
| `browser.list.arrow.up`, `browser.list.arrow.down` | sprite | Shown when more rows are above or below, in screen coordinates. |
| `browser.detail.pos` | | Position of the details panel. |
| `browser.detail.box` | box | Behind the details; its parts are cut off at the box when it is visible. |
| `browser.detail.name`, `browser.detail.comment` | text | Lobby name and comment (%s). |
| `browser.detail.code`, `browser.detail.host`, `browser.detail.rules` | text | Room ID (%s), host name and tag (%s, %s), rules summary (%s). |
| `browser.detail.member`, `browser.detail.member.spacing` | text | One line per member: name and tag (%s, %s), each `spacing` below the last. |

### Lobby settings

| Key | Type | Meaning |
|---|---|---|
| `settings.panel.<name>` | box | Boxes drawn behind the screen, in screen coordinates. |
| `settings.title.text.create`, `settings.title.text.edit` | text map | Title when creating and when editing. |
| `settings.menu` | menu | Items as listed under [Lobby settings](#lobby-settings); `item.value` shows each value; `add.key` and `subtract.key` change it. |
| `settings.info.text.<item>` | text map | Description of the highlighted item. |

### Lobby room: header and menus

| Key | Type | Meaning |
|---|---|---|
| `room.panel.<name>` | box | Boxes drawn behind the room in name order, in screen coordinates. |
| `room.title` | text | Lobby name (%s). |
| `room.comment` | text | Comment (%s). |
| `room.code` | text | Room ID (%s). |
| `room.count` | text | Members and size (%d, %d). |
| `room.rules` | text | Rules summary (%s). |
| `room.menu` | menu | Items `ready`, `chat`, `settings`, `start`, `watch`, `players`, `leave`; `valuename.unready`. |
| `room.playermenu` | menu | Items `mute`, `kick`, `host`, `back`; `valuename.unmute`. Drawn in place of the room menu. |

### Lobby room: member slots

| Key | Type | Meaning |
|---|---|---|
| `room.slot.pos`, `room.slot.spacing`, `room.slot.columns` | | Slot layout. Slots fill `columns` columns row by row; the x spacing separates columns and the y spacing separates rows. |
| `room.slot.bg.<kind>` | element | Behind each slot: `member`, `self` (this player's slot), `empty`, `closed`. Each kind has its own defaults, so a screenpack that restyles `member` restyles `self` too. |
| `room.slot.cursor` | element | Over the slot chosen with PLAYERS. |
| `room.slot.number` | text | Slot number (%d). |
| `room.slot.name` | text | Member name (%s; the tag follows as a second %s for a screenpack that wants both in one text). |
| `room.slot.tag` | text | The member's tag (%s), by default under the name. |
| `room.slot.empty`, `room.slot.closed` | text | Open slot, and slot beyond the lobby size. |
| `room.slot.host` | element | On the host's slot. |
| `room.slot.self` | element | On this player's slot. Not shown by default. |
| `room.slot.region` | text | Region (%s), when no flag is set for it. |
| `room.slot.connection.<type>` | element | Connection type: `wired`, `wifi`, `mobile`. Nothing is drawn when the type is unknown. |
| `room.slot.flag.<region>` | sprite | Flag for a region, keyed in lower case (for example `room.slot.flag.us.spr`). |
| `room.slot.record` | text | Wins and losses in this lobby (%d, %d). |
| `room.slot.badge.<state>` | element | `standby`, `ready`, `next`, `playing`, `watching`. |
| `room.slot.ping.*` | | Connection bars; see below. |

Connection bars:

| Key | Meaning |
|---|---|
| `room.slot.ping.thresholds` | The highest round trips (milliseconds) that still light 4, 3, 2 and 1 bars; a slower connection lights none. |
| `room.slot.ping.bars` | Number of bars. |
| `room.slot.ping.offset`, `room.slot.ping.spacing` | First bar, relative to the slot, and the step to the next bar. |
| `room.slot.ping.grow` | How much each further bar grows: x to the right, y upwards. |
| `room.slot.ping.on`, `room.slot.ping.off` | Boxes for lit and unlit bars. |
| `room.slot.ping.level.<n>` | Sprite or animation shown instead of the bars when n bars are lit (0 to `bars`). |
| `room.slot.ping.text` | Round trip in milliseconds (%d). Not shown by default. |
| `room.slot.ping.unknown` | Text shown before the member's first measurement. |
| `room.slot.ping.directon` | Box for lit bars when the round trip was measured over the players' direct connection; `on` is used when it is not visible. |
| `room.slot.ping.direct` | Element drawn with a direct round trip; its text receives the round trip (%d). Shows `P2P` by default. |

### Lobby room: status line, in-game panel and results panel

| Key | Type | Meaning |
|---|---|---|
| `room.status.text.<status>` | text map | Status line: `waiting`, `notready`, `waithost`, `waitall`, `waitentrant` (%s), `results` (%d seconds), `finished` (%s: the tournament winner), `finishedtie` (%s: the round-robin entrants who share the most wins, joined with `valuename.separator`). `kicked` and `closed` are shown in a message box when the room closes. |
| `room.match.pos` | | Position of the in-game panel. |
| `room.match.bg` | element | Panel background. |
| `room.match.title`, `room.match.vs` | text | Fixed texts. |
| `room.match.p1`, `room.match.p2` | text | Player names (%s). |
| `room.match.time` | text | Time since the match started (%d minutes, %02d seconds). |
| `room.match.streak` | text | Winner stays: the champion's straight wins and name (%d, %s). |
| `room.match.link.*` | | The players' round trip over their direct connection, once measured: the keys of the connection bars (`thresholds`, `bars`, `offset`, `spacing`, `grow`, `on`, `off`, `directon`, `direct`, `level.<n>`, `text`, `unknown`), relative to `room.match.pos`. By default, bars and `%d MS`. |
| `room.results.pos` | | Position of the results panel. |
| `room.results.bg` | element | Panel background. |
| `room.results.title.text.<key>` | text map | `win`, `nocontest`, `finished` (the final of a tournament). |
| `room.results.winner`, `room.results.loser` | text | Names (%s). |
| `room.results.streak` | text | The winner's straight wins (%d), from 2 on. |
| `room.results.reason.text.<reason>` | text map | Why a match had no winner, with both players' names (%s, %s): `draw`, `p2p` and `session_failed` (the players could not connect or start a session), `cancel`, `aborted` (the session broke off after it started), `left` (a player left the lobby), `timeout`; `default` for any other reason. |
| `room.results.next` | text | The next pairing (%s, %s). |
| `room.results.timer` | text | Seconds until the next match (%d). |

### Lobby room: chat

| Key | Type | Meaning |
|---|---|---|
| `room.chat.pos`, `room.chat.spacing`, `room.chat.lines` | | First log line, the step to the next line, and the number of lines shown. |
| `room.chat.box` | box | Behind the log, relative to `room.chat.pos`. Long lines wrap 4 units inside its right edge (300 units from `room.chat.pos` when the box is not visible). |
| `room.chat.name` | text | Sender's name and tag (%s, %s); the message text starts after it. |
| `room.chat.text` | text | Message text; its x offset is the gap after the name. |
| `room.chat.notice.text.<notice>` | text map | `joined`, `left`, `host`, `kicked`, `settings`, `start`; %s and %s are the member's name and tag (for `settings` and `start`, the host's). |
| `room.chat.error` | text | Errors from the server and failed connections. |
| `room.chat.input` | text | Chat entry (%s: the text typed so far), relative to `room.chat.pos`. |
| `room.chat.input.box` | box | Behind the chat entry, relative to its offset. |

## Background, music and related screens

- `[LobbyBgDef]` is the background of all lobby screens. A screenpack without it gets its `[OptionBgDef]`, or its title background when that is missing too.
- Message boxes (a failed request, a room that closed) use `[Warning Info]`. Backing out of a connection uses the title menu's cancel key and `cancel.snd` (`[Title Info]`).
- `[Music]` `lobby.bgm` (with the usual `.loop`, `.volume` and other bgm keys) plays in the lobby screens and again after each match. Without it, the title music keeps playing, and starts again after each match.
- The character select title of a lobby match is `[Select Info]` `title.netplaylobby.text` (default `Online Lobby`).
- The versus and victory screens follow the screenpack's versus-mode settings; the victory screen appears when `[Victory Screen]` `enabled` and `vs.enabled` are 1.

## Example: a room with two columns of member cards

This `[Lobby Info]` excerpt turns the default room into two columns of four member cards, marks the host's card with a gold edge instead of a HOST text, and moves the room menu and the player menu into a bar under the cards. Every key not listed keeps its default. Elements placed for the default one-row slots (the connection type, the WATCHING badge, the P2P mark) are placed on the cards too, or hidden.

```ini
[Lobby Info]
; Member slots as two columns of four cards.
room.slot.pos = 16, 62
room.slot.spacing = 150, 29
room.slot.columns = 2
room.slot.bg.member.box.coords = -6, -11, 142, 15
room.slot.bg.self.box.coords = -6, -11, 142, 15
room.slot.bg.empty.box.coords = -6, -11, 142, 15
room.slot.bg.closed.box.coords = -6, -11, 142, 15
room.slot.cursor.box.coords = -7, -12, 143, 16
; First line: number and name, then the tag. Second line: region, bars,
; record, badge.
room.slot.name.offset = 8, 0
room.slot.tag.offset = 8, 5
room.slot.empty.offset = 8, -1
room.slot.closed.offset = 8, -1
room.slot.region.offset = 8, 10
room.slot.ping.offset = 30, 11
room.slot.ping.unknown.offset = 30, 10
room.slot.record.offset = 48, 10
room.slot.badge.standby.offset = 118, 10
room.slot.badge.ready.offset = 118, 10
room.slot.badge.next.offset = 118, 10
room.slot.badge.playing.offset = 118, 10
room.slot.badge.watching.offset = 118, 10
; Connection type at the right of the first line; no P2P mark next to
; bars measured over a direct connection (they keep their own colour).
room.slot.connection.wired.offset = 118, 0
room.slot.connection.wifi.offset = 118, 0
room.slot.connection.mobile.offset = 118, 0
room.slot.ping.direct.text =
; The host's card gets a gold edge instead of the HOST text.
room.slot.host.text =
room.slot.host.offset = 0, 0
room.slot.host.box.visible = 1
room.slot.host.box.coords = -6, -11, -5, 15
room.slot.host.box.col = 255, 214, 64

; Room menu and player menu as a bar under the cards.
room.panel.menubar.visible = 1
room.panel.menubar.coords = 4, 169, 316, 184
room.panel.menubar.col = 0, 0, 0
room.panel.menubar.alpha = 0, 112
room.menu.pos = 32, 180
room.menu.item.font = f-6x9.def, 0, 0, 160, 160, 176, 255, -1
room.menu.item.active.font = f-6x9.def, 0, 0, 255, 255, 255, 255, -1
room.menu.item.spacing = 51, 0
room.menu.boxcursor.coords = -27, -10, 26, 2
room.menu.boxbg.visible = 0
room.menu.next.key = R
room.menu.previous.key = L
room.playermenu.pos = 40, 180
room.playermenu.item.font = f-6x9.def, 0, 0, 160, 160, 176, 255, -1
room.playermenu.item.active.font = f-6x9.def, 0, 0, 255, 255, 255, 255, -1
room.playermenu.item.spacing = 60, 0
room.playermenu.boxcursor.coords = -30, -10, 29, 2
room.playermenu.boxbg.visible = 0
room.playermenu.next.key = R
room.playermenu.previous.key = L

; The results panel over the middle of the cards.
room.results.pos = 160, 112
```

The room menu's `next.key` and `previous.key` also move the PLAYERS cursor, so with this layout left and right step through the cards.

Sprites replace boxes and texts the same way. With the screenpack's own sprites (the numbers below are placeholders):

```ini
[Lobby Info]
; Badges as sprites: the box and text of each badge are turned off.
room.slot.badge.ready.spr = 700, 1
room.slot.badge.ready.text =
room.slot.badge.ready.box.visible = 0
; Flags instead of region codes; regions come from each player's server profile.
room.slot.flag.us.spr = 710, 0
room.slot.flag.jp.spr = 710, 1
; Signal-strength art instead of bars.
room.slot.ping.level.0.spr = 720, 0
room.slot.ping.level.1.spr = 720, 1
room.slot.ping.level.2.spr = 720, 2
room.slot.ping.level.3.spr = 720, 3
room.slot.ping.level.4.spr = 720, 4
```

## Script hooks and tunables

`lobby.lua` is loaded as the global `lobby` before the mods in `external/mods`, so a mod can change these fields when it loads:

| Field | Default | Meaning |
|---|---|---|
| `lobby.choices.<item>` | see [Lobby settings](#lobby-settings) | Values offered for each settings item. The server accepts size 2–8, max games 1–99, rounds 0–9, round time 10–999 seconds (0 default, -1 no limit), interval 3–30 seconds and spectators 0–10 seconds (0 off). |
| `lobby.defaultSettings` | queue, 8 players, spectators 3 seconds | Settings a new lobby starts from. After the first lobby, lobby settings starts from the last settings used. |
| `lobby.requestFrames` | 720 | Frames to wait for the server before giving up. |
| `lobby.pingInterval` | 180 | Frames between round-trip measurements. |
| `lobby.reportInterval` | 900 | A new round trip is reported to the lobby when its bar count changes, or after this many frames when it moved by 20 ms or more. |
| `lobby.inputGuardFrames` | 12 | Frames of ignored input after a screen change. |
| `lobby.afterMatchGuardFrames` | 60 | Frames of ignored input after a match, so presses meant for the victory screen do not select READY. |
| `lobby.listDelayFrames` | 75 | Frames lobby search waits before refreshing after a room is left, so the list no longer shows the player in it (Nakama updates the listing about once a second). |
| `lobby.chatHistory` | 50 | Chat lines kept. |
| `lobby.chatLimit` | 80 | Longest chat message. |
| `lobby.nameLength` | `{2, 16}` | Shortest and longest online name, checked before the name is sent; the server checks the same. The lifebar's name fonts decide which characters a name can have. |
| `lobby.accountFrames` | 2100 | Frames to wait for the server to save a new online name. |
| `lobby.watchFrames` | 1800 | Frames watching waits for the match's stream to become playable. |

The hook list `lobby.f_setupMatch` runs after the lobby has set up a match's rules, with the lobby's settings table as its argument:

```lua
hook.add('lobby.f_setupMatch', 'mymod', function(settings)
	-- settings.rounds, settings.time, settings.teams, settings.stage, ...
end)
```

Both players' games run it before the same match, and a netplay session only stays in sync if both make the same changes; a hook must therefore depend only on the settings table and on data that is the same on both machines.

`lobby.f_rules(settings, separator)` returns the rules summary described under [Value names](#value-names), for screenpacks that draw their own lobby elements.

## Limits

- For members who have not played each other, the connection bars show the round trip to the online server, which only approximates the connection between them.
- The connection type is reported by each member's game and is not verified; it describes only the link from the member's machine to the next device.
- A spectator sees the match at least one second after the players (the spectators setting), needs the players' game content, and does not see the victory and results screens.
- The default fonts other than the chat's are small bitmap fonts that draw printable ASCII only, and `f-4x6` draws most lowercase letters as capitals. The chat's M PLUS 1 draws Latin and Japanese, not other scripts. [Fonts](#fonts) lists what the fonts that show names must cover.
- Text entry (lobby name, comment, room ID, chat) needs a keyboard.
- A tag is four digits taken from the account ID: two accounts share one about once in 10,000 pairs, and since new accounts are cheap, a tag identifies an account without proving who plays it.
