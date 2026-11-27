# I.K.E.M.E.N-Go for iOS

## Instructions for building
### Requirements: XCode, iOS SDK, MoltenVK SDK (Homebrew is ideal)
1. Clone this repo. You'll need it to build I.K.E.M.E.N-Go for iOS.
2. Build the main I.K.E.M.E.N-Go engine for iOS using the build script included with the engine (`build/build.sh iOS`). You will need to point the environment variable `IOS_WRAPPER_DIR` to the location of this repo before running `build/build.sh iOS`.
3. The build from Step 1 will automatically copy all the necessary library dependencies **except** for SDL2 and MoltenVK.
4. Build SDL2 2.32.10 for iOS and place the `SDL2.xcframework` inside the `Frameworks` folder.
5. Download MoltenVK for iOS and place the `MoltenVK.xcframework` from the `dynamic` folder into the `Frameworks` folder.
6. Place all the engine default assets (screenpack, chars, etc.) according to `DefaultAssets/manifest.txt` in `DefaultAssets` and zip up all the directories and files included in this repo IN THE ROOT OF A FILE CALLED `DefaultAssets.zip` and **place it in the root of this project (alongside the `.a` files)**. For instance, `stages`, `chars`, `data`, etc. should all be in the ROOT of `DefaultAssets.zip` ALONGSIDE `manifest.txt` which needs to be included in `DefaultAssets.zip` as well. Do NOT put anything in a subdirectory called `DefaultAssets` in the root of the ZIP or it will not work! GOOD✅: `DefaultAssets.zip/chars` BAD❌: `DefaultAssets.zip/DefaultAssets/chars`
7. Open the I.K.E.M.E.N-Go.xcodeproj in XCode.
8. In XCode, select the device you wish to provision in the top menu and hit the Play button.

NOTE: ***Simulator builds are not supported.*** This is because MoltenVK does not have a simulator build.

Due to Apple iOS restrictions, you are responsible for signing your own modifications.

### KNOWN LIMITATIONS
* Bluetooth game controllers must be paired through the OS and turned on before the loading screen appears.
* Orientation locks on initial orientation (to mask a rotation bug)

## LICENSE NOTICE
© 2026 Jesuszilla

This project uses [ZipArchive](https://github.com/ZipArchive/ZipArchive) as a dependency which is licensed under the MIT License.

Special thanks:
* Lasombra Demon - Spanish translations
* MotorRoach - Portuguese translations
* Vans - Chinese & Japanese translation feedback, Spanish iOS game folder dialog translations
* dionednd - Filipino translations
