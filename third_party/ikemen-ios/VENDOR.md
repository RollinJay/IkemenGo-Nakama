# ikemen-ios in this repository

This folder holds the iOS app that `build/build.sh iOS` packages the game into. It's an Xcode project (Objective-C) around the engine's `libikemen.a`, with Jesuszilla's start-up, game folder picker, asset extraction and touch controls.

- Source: https://github.com/Jesuszilla/ikemen-ios, branch `main`
- Commit: 60606d640839ad72a20d3b45a5c7aa7939a446ce (2026-10-17)
- License: zlib, in `LICENSE.txt`, with the notices in `README.md`

`build/build.sh` copies this folder to `build/ios-app/ikemen-ios` (`IOS_APP_REPO`). It stages the engine's library, its header and the game's assets in that copy, so building never changes this folder. `IOS_APP_REPO` can still name another checkout or a git URL, and `IOS_WRAPPER_DIR` a checkout to build in place.

## Local changes

They make the on-screen buttons work as the Android app's do (see `third_party/ikemen-droid/VENDOR.md`).

- **`TouchOverlayView`:**
  - The on-screen buttons play as a controller named "IKEMEN Touch Controls" (`SDL_JoystickAttachVirtualEx`, no vendor or product ID), so that the game recognises it.
  - The buttons can be hidden (`setControlsShown:`), which also removes their controller. Hidden, they take no touches, and the next touch after they show again brings the controller back.
  - Each attack, Start and Menu button shows the glyph image the game writes for it, or its letter where there is none (`loadGlyphsFromDirectory:`). The d-pad is a single disc and shows none.
  - The game chooses which of the ten buttons show (`setButtonsShown:`). Hidden grid buttons keep their place, and the shown ones in their row move right over the gaps; Start and Menu close up. Hidden buttons take no touches. The game can also move Start into the grid, in a hidden button's place (`[Touch Controls] start.slot`). With `[Touch Controls] layout = grid` the grid drops its stagger: each punch sits straight above its kick, and Start, in the grid, stands apart on the right. Buttons move by transforms, so the xib's layout stays as it is.
- **`TouchControls`** (new):
  - **Setting:** Automatic, Always show or Always hide, chosen from a small settings button at the top left. Automatic hides the buttons while a real controller is connected: once the game runs, one it has (command 0x8004); before, one the GameController framework lists (its notifications, and a check every second). The framework's list may stand still while the game holds the main queue.
  - **Keyboard button:** shown while the game has a text box open and the keyboard is closed.
  - **From the game:** `IkemenTouchControlsMessage` takes commands 0x8001 (a text box opened or closed), 0x8002 (the glyph images changed), 0x8003 (the buttons to show, remembered for the next start) and 0x8004 (the number of real controllers the game has).
  - **To the game:** `IkemenShowKeyboard`, and `IkemenSetKeyboardCover` with how much of the game's view the keyboard covers; the game draws itself above it.
  - **Text field:** SDL's text field is in SDL's window, under the app's, so SDL's window is the key window while a text box is open.
  - Nothing in it goes through the main dispatch queue, which the game holds while it runs.
- **Bundled files (`ViewController extractZipAssetsToURL:`):** the same triggers as before (first run, a new `CurrentAssetsVersion`, or `external/script` differing from the app's), but no longer an unzip over the game folder. `DefaultAssets.zip` goes to a temporary folder; `external/script` is always replaced from it, and every other file is copied only where the game folder has none, as the Android app's `AssetExtractor` does. A game's own `data/system.def`, `select.def`, `fight.def`, chars, stages, sounds and the rest are never overwritten, a version bump included.
- **`ViewController`:** gives the game `IkemenTouchControlsMessage` (`IkemenSetAppMessageHandler`) before `SDL_main`, and hides the status bar.
- **`TouchOverlayViewController`:** hands the overlay to `TouchControls` once the xib is loaded.
