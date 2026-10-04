package main

// Screenpack parameters for the online lobby screens ([Lobby Info] and
// [LobbyBgDef]). external/script/lobby.lua draws the screens; every element
// uses the engine's usual motif property types, so fonts, sprites,
// animations, localcoord and per-language sections work as elsewhere.
//
// Positions: all lobby coordinates use [Lobby Info] localcoord (320, 240 by
// default). An element's offset is relative to the position of the thing it
// belongs to (a list row, a slot, a panel's pos). Elements that belong to
// nothing (titles, the room ID) use screen coordinates, as in other screens.

// LobbyBoxProperties is a filled rectangle. coords are x1, y1, x2, y2
// relative to the position of the element that owns the box. Nothing is
// drawn unless visible = 1.
type LobbyBoxProperties struct {
	Visible    bool     `ini:"visible"`
	Coords     [4]int32 `ini:"coords"`
	Col        [3]int32 `ini:"col"`
	Alpha      [2]int32 `ini:"alpha" default:"255,0"`
	Layerno    int16    `ini:"layerno"`
	Localcoord [2]int32 `ini:"localcoord"`
	RectData   *Rect
}

// LobbyElementProperties is an animation or sprite and a text that share one
// offset, drawn over an optional box (its coords are relative to that
// offset). Any of the three can be left unset.
type LobbyElementProperties struct {
	AnimationTextProperties
	Box LobbyBoxProperties `ini:"box"`
}

type LobbyBrowserProperties struct {
	// panel.<name>: boxes drawn behind everything else on the screen, in
	// name order; coords are screen coordinates.
	Panel map[string]*LobbyBoxProperties `ini:"panel"`
	Title TextProperties                 `ini:"title"`
	// Menu items: join, search, create, code, filter, account, back.
	Menu    MenuProperties `ini:"menu"`
	Count   TextProperties `ini:"count"`   // %d: number of lobbies listed
	Account TextProperties `ini:"account"` // %s: this player's online name
	List    struct {
		Pos          [2]float32             `ini:"pos"`
		Spacing      [2]float32             `ini:"spacing"`
		VisibleItems int32                  `ini:"visibleitems" default:"5"`
		Box          LobbyBoxProperties     `ini:"box"` // panel behind the whole list
		Header       TextProperties         `ini:"header"`
		Row          LobbyElementProperties `ini:"row"`
		Active       LobbyElementProperties `ini:"active"`
		Name         TextProperties         `ini:"name"`    // %s
		Comment      TextProperties         `ini:"comment"` // %s
		Players      TextProperties         `ini:"players"` // %d, %d: members, size
		Format       TextProperties         `ini:"format"`  // %s: valuename of the format
		// text.<phase>: waiting, playing, results, finished; text.full
		// replaces it while the lobby is full.
		Phase TextMapProperties `ini:"phase"`
		Code  TextProperties    `ini:"code"` // %s: room ID
		Host  TextProperties    `ini:"host"` // %s: host name
		Empty TextProperties    `ini:"empty"`
		// Keys that move through the list; the menu keys move along the menu.
		Next struct {
			Key []string `ini:"key"`
		} `ini:"next"`
		Previous struct {
			Key []string `ini:"key"`
		} `ini:"previous"`
		Arrow struct {
			Up   AnimationProperties `ini:"up"`
			Down AnimationProperties `ini:"down"`
		} `ini:"arrow"`
	} `ini:"list"`
	Detail struct {
		Pos     [2]float32         `ini:"pos"`
		Box     LobbyBoxProperties `ini:"box"`
		Name    TextProperties     `ini:"name"`    // %s
		Comment TextProperties     `ini:"comment"` // %s
		Code    TextProperties     `ini:"code"`    // %s
		Host    TextProperties     `ini:"host"`    // %s
		Rules   TextProperties     `ini:"rules"`   // %s: rules summary
		Member  struct {
			TextProperties
			Spacing [2]float32 `ini:"spacing"`
		} `ini:"member"`
	} `ini:"detail"`
}

type LobbySettingsProperties struct {
	Panel map[string]*LobbyBoxProperties `ini:"panel"`
	Title TextMapProperties              `ini:"title"` // text.create, text.edit
	// Menu items: name, comment, size, format, maxgames, rounds, time,
	// teams, stage, start, interval, type, watch, confirm, back.
	Menu MenuProperties    `ini:"menu"`
	Info TextMapProperties `ini:"info"` // text.<item>: description of the highlighted item
}

type LobbyPingProperties struct {
	// thresholds: the highest round trip (ms) that still shows 4, 3, 2 and 1
	// bars; slower connections show 0 bars.
	Thresholds []int32                         `ini:"thresholds" default:"60,120,200,300"`
	Bars       int32                           `ini:"bars" default:"4"`
	Offset     [2]float32                      `ini:"offset"`
	Spacing    [2]float32                      `ini:"spacing"`
	Grow       [2]float32                      `ini:"grow"`
	On         LobbyBoxProperties              `ini:"on"`
	Off        LobbyBoxProperties              `ini:"off"`
	Level      map[string]*AnimationProperties `ini:"level"` // level.<0-4>: sprite instead of bars
	Text       TextProperties                  `ini:"text"`  // %d: round trip in ms
	Unknown    TextProperties                  `ini:"unknown"`
	// A round trip measured over two players' direct connection: directon
	// replaces on for the lit bars (when visible), and direct is drawn too
	// (its text receives the round trip, %d).
	DirectOn LobbyBoxProperties     `ini:"directon"`
	Direct   LobbyElementProperties `ini:"direct"`
}

type LobbySlotProperties struct {
	Pos     [2]float32 `ini:"pos"`
	Spacing [2]float32 `ini:"spacing"`
	Columns int32      `ini:"columns" default:"1"`
	// bg.<kind>: behind each slot; kinds: member, self (this player's slot),
	// empty, closed.
	Bg     map[string]*LobbyElementProperties `ini:"bg"`
	Cursor LobbyElementProperties             `ini:"cursor"`
	Number TextProperties                     `ini:"number"` // %d
	Name   TextProperties                     `ini:"name"`   // %s
	Tag    TextProperties                     `ini:"tag"`    // %s: the member's tag (account ID digits)
	Empty  TextProperties                     `ini:"empty"`
	Closed TextProperties                     `ini:"closed"`
	Host   LobbyElementProperties             `ini:"host"`
	Self   LobbyElementProperties             `ini:"self"`
	// badge.<state>: standby, ready, next, playing, watching.
	Badge  map[string]*LobbyElementProperties `ini:"badge"`
	Ping   LobbyPingProperties                `ini:"ping"`
	Region TextProperties                     `ini:"region"` // %s
	// connection.<type>: wired, wifi, mobile (nothing is drawn when the
	// member's connection type is unknown).
	Connection map[string]*LobbyElementProperties `ini:"connection"`
	// flag.<region>: sprite or animation per region code.
	Flag   map[string]*AnimationProperties `ini:"flag"`
	Record TextProperties                  `ini:"record"` // %d, %d: wins, losses
}

type LobbyRoomProperties struct {
	Panel   map[string]*LobbyBoxProperties `ini:"panel"`
	Title   TextProperties                 `ini:"title"`   // %s: lobby name
	Comment TextProperties                 `ini:"comment"` // %s
	Code    TextProperties                 `ini:"code"`    // %s: room ID
	Rules   TextProperties                 `ini:"rules"`   // %s: rules summary
	Count   TextProperties                 `ini:"count"`   // %d, %d: members, size
	// Menu items: ready, chat, settings, start, watch, players, leave.
	Menu MenuProperties `ini:"menu"`
	// Actions on a chosen member: mute, kick, host, back.
	PlayerMenu MenuProperties      `ini:"playermenu"`
	Slot       LobbySlotProperties `ini:"slot"`
	// text.<status>: waiting, notready, waithost, waitall, waitentrant (%s),
	// results (%d), finished (%s), finishedtie (%s); kicked and closed are
	// shown in a message box when the room closes.
	Status TextMapProperties `ini:"status"`
	Match  struct {
		Pos    [2]float32             `ini:"pos"`
		Bg     LobbyElementProperties `ini:"bg"`
		Title  TextProperties         `ini:"title"`
		P1     TextProperties         `ini:"p1"` // %s
		P2     TextProperties         `ini:"p2"` // %s
		Vs     TextProperties         `ini:"vs"`
		Time   TextProperties         `ini:"time"`   // %d, %02d: minutes, seconds
		Streak TextProperties         `ini:"streak"` // %d, %s: straight wins, champion
		// The players' round trip over their direct connection, once measured.
		Link LobbyPingProperties `ini:"link"`
	} `ini:"match"`
	Results struct {
		Pos    [2]float32             `ini:"pos"`
		Bg     LobbyElementProperties `ini:"bg"`
		Title  TextMapProperties      `ini:"title"`  // text.win, text.nocontest, text.finished
		Winner TextProperties         `ini:"winner"` // %s
		Loser  TextProperties         `ini:"loser"`  // %s
		Streak TextProperties         `ini:"streak"` // %d
		Reason TextMapProperties      `ini:"reason"` // text.<reason>, text.default: %s, %s: the players
		Next   TextProperties         `ini:"next"`   // %s, %s: the next pairing
		Timer  TextProperties         `ini:"timer"`  // %d
	} `ini:"results"`
	Chat struct {
		Pos     [2]float32         `ini:"pos"`
		Spacing [2]float32         `ini:"spacing"`
		Lines   int32              `ini:"lines" default:"4"`
		Box     LobbyBoxProperties `ini:"box"`
		Name    TextProperties     `ini:"name"` // %s
		Text    TextProperties     `ini:"text"`
		Notice  TextMapProperties  `ini:"notice"` // text.<notice>: %s is the member
		Error   TextProperties     `ini:"error"`
		Input   struct {
			TextProperties                    // %s: the text typed so far
			Box            LobbyBoxProperties `ini:"box"`
		} `ini:"input"`
	} `ini:"chat"`
}

type LobbyInfoProperties struct {
	// Coordinate space of every lobby position, offset and box. An element
	// may still set its own localcoord.
	Localcoord [2]int32       `ini:"localcoord"`
	FadeIn     FadeProperties `ini:"fadein"`
	FadeOut    FadeProperties `ini:"fadeout"`
	Cursor     struct {
		Move struct {
			Snd [2]int32 `ini:"snd" default:"-1,0"`
		} `ini:"move"`
		Done struct {
			Snd [2]int32 `ini:"snd" default:"-1,0"`
		} `ini:"done"`
	} `ini:"cursor"`
	Cancel struct {
		Snd [2]int32 `ini:"snd" default:"-1,0"`
	} `ini:"cancel"`
	// event.snd.<event>: join, leave, ready, chat, match, error.
	Event struct {
		Snd map[string][2]int32 `ini:"snd" default:"-1,0" keyfirst:"true"`
	} `ini:"event"`
	// Full-screen status while waiting on the server: text.<status>.
	Status struct {
		TextMapProperties
		Overlay OverlayProperties `ini:"overlay"`
	} `ini:"status"`
	TextInput TextInputProperties `ini:"textinput"`
	// valuename.<value>: display names of formats and setting values.
	Valuename map[string]string `ini:"valuename"`
	// message.<key>: texts shown in the warning box (nameinvalid, namefont,
	// namefailed, watchfailed, watchcontent).
	Message  map[string]string       `ini:"message"`
	Browser  LobbyBrowserProperties  `ini:"browser"`
	Settings LobbySettingsProperties `ini:"settings"`
	Room     LobbyRoomProperties     `ini:"room"`
}
