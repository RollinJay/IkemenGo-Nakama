# Replay++ Module for IKEMEN GO

This is a module for the IKEMEN GO engine that adds more functionality to the
replay game mode. It adds the following features:
* Fast forward and rewind
* Pause and frame stepping
* Replay takeover (play as P1 or P2)

As seen in commercial fighting games.

## Installation

Extract the `jayreplay++` folder directly into the folder you wish to store it in.
You must place it in `external/mods`.

Next, extract the `Ikemen_GO-Replay++.exe` file and place it in the root of your IKEMEN
GO installation folder. You may choose to either replace your existing executable or
keep them separate.

For editing the replay.go file portion of the module and making a new `Ikemen_GO-Replay++.exe`
file, please read [BUILDING.md](https://github.com/ikemen-engine/Ikemen-GO/blob/develop/BUILDING.md) in the IKEMEN GO page.

To enable input display compatibility, you must install [jayinputdisplay](https://github.com/saintjr2004/Ikemen-GO-Advanced-Input-Display) into your build as well.

## Disclaimer
Since this is a very experimental module, it may be prone to various edge case bugs and engine quirks. Feedback is greatly appreciated.

## Known Limitations

1. You cannot rewind to a previous round. Game states are automatically consumed upon round end.
2. Rewinding in general is somewhat choppy. It smooths out as more game states are saved, but rewinding especially during newly visited portions of a replay may appear to be choppy.
3. Stock fast forward is still enabled during character select and other non-fight screens. It is not disabled entirely, and doing so requires editing netplay.go, which I do not want to do as I want this module to strictly add scripts, not modify existing ones.

# New Lua Trigger Documentation

This module adds various new lua functions in order for the replays to be modified. These functions will be 
documented for future reference or use in other builds.

### `replayIsPlaying`

```lua
replayIsPlaying()
```

Returns whether the engine is in an eligible replay match gameplay state, with neither the native menu open nor a takeover active. This checks the engine's match eligibility conditions, not whether the replay is physically advancing.

The eligibility check requires a loaded replay file, a running match and stage, no active incompatible connection or rollback session, and no listed non-fight screens or match-ending flags.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayIsPlaying` | `boolean` | Returns `true` when eligible replay match gameplay is active, otherwise it is `false` |

### `replayMenuPhase`

```lua
replayMenuPhase()
```

Returns the native pause menu's current phase if a replay file is loaded and the menu is active. This function reports the native menu state.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `"openingout"` | `string` | Opening the menu: outgoing fade |
| 2 | `"openingin"` | `string` | Opening the menu: incoming fade |
| 3 | `"open"` | `string` | Menu fully open |
| 4 | `"closingout"` | `string` | Closing the menu: outgoing fade |
| 5 | `"closingin"` | `string` | Closing the menu: incoming fade |
| 6 | `""` | `string` | No active native menu or no replay file |

### `replayPause`

```lua
replayPause()
```

Toggles the replay's logical pause state during eligible normal replay gameplay. It does nothing if the replay is outside an eligible match, the menu is active, or a takeover is active. If a rewind-created temporary pause is active, the function instead converts that state into a pause that remains after the rewind key is released.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayPause` | `nil` | Returns nothing. |

### `replayRewind`

```lua
replayRewind()
```

Requests a rewind by the number of frames configured in `rewind.speed`. Restores a valid snapshot from the current round and re-simulates recorded frames forward to the exact target if needed. It cannot rewind beyond its oldest valid current-round checkpoint.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayRewind` | `boolean` | Returns `true` if a rewind seek was accepted and the snapshot was restored, `false` if no eligible rewind was possible. A `true` result does not automatically mean the exact target has already finished re-simulating, but rather it found a valid restoration point. |

### `replayFastForward`

```lua
replayFastForward(frames)
```

Requests an exact-target forward seek of `frames` game ticks from the current frame.

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `frames` | `int` | Number of frames to advance. Min-capped at 1. |

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayFastForward` | `boolean` | Returns `true` if the seek was started, otherwise returns `false` if the match is not eligible, the menu or takeover is active, or another seek is running. The target is reached asynchronously through subsequent match updates. |

### `replayMenuInput`

```lua
replayMenuInput(players, commands, ...)
```

Checks physical player input, rather than the recorded replay command buffer, while the native replay pause menu is active. This is used to circumvent human input being traditionally unavailable during replays.

Menu direction events use the configured UI repeat delay and rate due to alternate input handling. Successfully consumed events are not returned a second time during the same menu-input sequence. An example of a multi-input declaration of the function would look like this:

```lua
if replayMenuInput(1, {'a', 'b', 'c', 'x', 'y', 'z', 's'}) then
    print('Replay operator selected the menu item')
end
```

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `players` | any | Compatibility argument matching `getInput`. Physical replay-menu inputs come from the controller selected with `replaySetController()`, not from this argument. |
| 2+ | `commands` | `string` or `table` | One or more command names, or a table of command names to test. Valid names include `U`, `D`, `L`, `R`, `a`, `b`, `c`, `x`, `y`, `z`, `s`, `m`, `B`, `F`, `LS_X-`, `LS_X+`, `LS_Y-`, `LS_Y+`, `d` and `w`. |

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayMenuInput` | `boolean` | Returns `true` if a requested command has an available press/repeat event, otherwise returns `false`. |

### `replaySetController`

```lua
replaySetController(controller)
```

Sets which physical player's configured keyboard/gamepad bindings control replay navigation, pausing, rewinding, fast-forwarding, and takeover input. The selected controller also provides input to the Replay Menu. This selects the human replay operator, not the fighter side being taken over with `replayBegin(side)`.

`replay.lua` calls this function after a recording is selected. The Go replay manager starts with controller 1 by default. The function updates the replay manager's input source and synchronizes its menu and pause key-press tracking with the selected controller.

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `controller` | `int` | Physical controller slot: `1` for P1 or `2` for P2. Any value other than `2` is treated as `1`. |

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replaySetController` | `int` | Returns the selected controller slot (`1` or `2`). |

### `replayOpen`

```lua
replayOpen()
```

Opens the native Replay Menu during eligible replay gameplay. If a takeover is active, it requests a return to the saved takeover-start state through the menu transition instead.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayOpen` | `boolean` | Returns `true` when the open/return request was accepted, otherwise returns `false` (for example, when the menu is already open or no eligible match is running). |

### `replayBegin`

```lua
replayBegin(side)
```

Requests takeover of one replay side from the open Replay Menu. The engine queues the takeover and applies it after the menu has finished closing. Physical controls are routed to the chosen side. The original replay recording is not permanently modified.

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `side` | `int` | Valid values are `1` and `2` for P1 and P2 sides respectively. |

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayBegin` | `boolean` | Returns `true` if the takeover request was queued, or `false` if the side is invalid, no menu-origin snapshot exists, the menu is not active, or a reset is pending. |

### `replayReturn`

```lua
replayReturn()
```

Requests a return from an active takeover to its saved origin. Opens the native pause menu and defers snapshot restoration until the appropriate menu transition phase. If there is no active takeover or valid origin snapshot, this function does nothing.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayReturn` | `nil` | Returns nothing. |

### `replayRequestExit`

```lua
replayRequestExit()
```

Marks the replay for exit. It does not immediately terminate the replay, but prepares the replay for it.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayRequestExit` | `boolean` | Returns `true` when accepted, otherwise returns `false` if the menu is not open, a normal replay origin is missing, a takeover or pending takeover exists, or an exit is already requested. |

### `replayIncludeCommonState`

```lua
replayIncludeCommonState(statePath, fxPath)
```

Adds a locally installed ZSS common-state file and its matching FX definition to the active replay configuration. For whatever reason, some modules may not loaded during replays, so this function is to ensure that specifically the input display module is loaded.

The function preserves the replay's existing common-state and FX entries. Temporary entries are removed during `replayClean()` if they are still in the expected position.

Both paths are required. Call this after `enterReplay()` has successfully opened the replay and before the match loads its common-state and FX resources.

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `statePath` | `string` | Path to the ZSS file to register under `Common.States`. Must not be empty. |
| 2 | `fxPath` | `string` | Path to the matching FX definition registered under `Common.Fx`. Must not be empty. |

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayIncludeCommonState` | `boolean` | Returns `true` when a replay file is open and both paths are supplied, otherwise `false`. `true` means the configuration entries were processed; it does not verify that the files compile successfully. |

### `replayInstall`

```lua
replayInstall()
```

Registers the module's per-frame Lua callback in `sys.cfg.Common.Lua`, under `zz_replay_takeover`:

```lua
require('external.mods.jayreplay++.replay').tick()
```

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayInstall` | `boolean` | Returns `true` upon success. |

### `replayConfigure`

```lua
replayConfigure(maxTakeoverTicks, historyInterval, historyCount,
    rewindFrames, menuCommand, pauseCommand, rewindCommand,
    rewindRepeat, fastForwardCommand, fastForwardFrames,
    fastForwardRepeat, holdDelay, fastForwardRate)
```

Configures the native replay manager. In the module these values are all set based on values in `replay.def` but can be manually configured.

**Parameters**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `maxTakeoverTicks` | `number` | Maximum takeover duration in game ticks. Clamped to 1–1800. |
| 2 | `historyInterval` | `number` | Minimum game ticks between regularly captured rewind checkpoints. Min-capped at 1. |
| 3 | `historyCount` | `number` | Maximum retained rewind checkpoints. Min-capped at 2. |
| 4 | `rewindFrames` | `number` | Frames per direct `replayRewind()` request and held rewind operation. Min-capped at 1. |
| 5 | `menuCommand` | `string` | Command used to open the menu e.g. `"m"` |
| 6 | `pauseCommand` | `string` | Pause toggle command e.g. `"s"`. |
| 7 | `rewindCommand` | `string` | Rewind command e.g. `"L"`. |
| 8 | `rewindRepeat` | `number` | Minimum held-rewind repeat interval. Min-capped at 1. |
| 9 | `fastForwardCommand` | `string` | Fast-forward command e.g. `"R"`. |
| 10 | `fastForwardFrames` | `number` | Frames per held forward seek. Clamped to 1–36000. |
| 11 | `fastForwardRepeat` | `number` | Minimum held-forward repeat interval. Min-capped at 1. |
| 12 | `holdDelay` | `number` | Hold threshold in ticks. Min-capped at 2. |
| 13 | `fastForwardRate` | `number` | Unpaused playback acceleration multiplier. Clamped to 1–16. |

Note: Command names are case-sensitive: `R`, `L`, `U`, `D`, `x`, `y`, `z`, `a`, `b`, `c`, `s`, `m`. Parameters #5–13 are conditionally applied if supplied.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayConfigure` | `nil` | Returns nothing. |

### `replayTick`

```lua
replayTick()
```

Runs a native replay-manager update and checks whether a previously requested replay exit has finished its menu fade. The exit-ready flag is consumed when read. Calling this function in extra update loops can consume the exit flag or update replay states unexpectedly, so generally leave it alone.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayTick` | `boolean` | Returns `true` once when a deferred exit becomes ready, otherwise returns `false`. |

### `replayFinishExit`

```lua
replayFinishExit()
```

Performs final replay-exit cleanup, resets the native round fade, resets the UI token guard, and marks the match as ended. It does not wait for fade completion itself. Ideally used only after the deferred exit has been reported ready.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayFinishExit` | `nil` | Returns nothing. |

### `replayClean`

```lua
replayClean()
```

Cleans up the replay manager. This restores temporarily masked input bindings and pause-menu settings, closes/deletes any temporary takeover recording, releases saved snapshots, and clears replay runtime flags. It also removes temporary common-state and FX entries added by `replayIncludeCommonState()` when their original positions remain intact. Generally avoid calling it during a live takeover unless ending the replay is intended.

**Returns**

| # | Name | Type | Description |
| --- | --- | --- | --- |
| 1 | `replayClean` | `nil` | Returns nothing. |
