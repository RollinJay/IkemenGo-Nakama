# ikemen-ios in this repository

This folder holds the iOS app that `build/build.sh iOS` packages the game into. It's an Xcode project (Objective-C) around the engine's `libikemen.a`, with Jesuszilla's start-up, game folder picker, asset extraction and touch controls.

- Source: https://github.com/Jesuszilla/ikemen-ios, branch `main`
- Commit: 60606d640839ad72a20d3b45a5c7aa7939a446ce (2026-10-17)
- License: zlib, in `LICENSE.txt`, with the notices in `README.md`

`build/build.sh` copies this folder to `build/ios-app/ikemen-ios` (`IOS_APP_REPO`). It stages the engine's library, its header and the game's assets in that copy, so building never changes this folder. `IOS_APP_REPO` can still name another checkout or a git URL, and `IOS_WRAPPER_DIR` a checkout to build in place.

## Local changes

None yet.
