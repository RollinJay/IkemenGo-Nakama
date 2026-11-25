//
//  ViewController.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/24/26.
//

#import "GamepadInputs.h"
#import "ViewController.h"
#import "TouchOverlayView.h"
#import "TouchOverlayViewController.h"
#import "AnalogJoystickView.h"
#import <Foundation/Foundation.h>
#import <GameController/GameController.h>
#import <UniformTypeIdentifiers/UniformTypeIdentifiers.h>
#import "zlib.h"
#import "ZipArchive.h"
#import "libikemen.h"
#import <SDL2/SDL.h>
#import "GoWindow.h"
#import "SDLWindowFix.h"
#import "TouchControls.h"

@interface ViewController ()
@property (nonatomic, strong) NSURL *assetsFolderURL;
@property (nonatomic, assign) BOOL securityScopeActive;
@property (nonatomic, assign) BOOL ikemenStarted;
@property (nonatomic, strong) UIDocumentPickerViewController *activePicker;
@property (nonatomic, strong) UIWindow *alertWindow;
@property (nonatomic, strong) TouchOverlayView *overlayView;
@property (nonatomic, strong) MetalContainerView *metalContainerView;
@end

@implementation ViewController

// Assets folder bookmark
static NSString * const AssetsFolderBookmarkKey = @"IKEMENAssetsFolderBookmark";
static NSString * const AssetsInstallMarkerName = @".ikemen-assets-version";
static NSString * const CurrentAssetsVersion = @"3";

// Game folder workaround shit
static NSString * const RestartFolderPickKey = @"ShouldDisplayRestartFolderPicker";

- (void)setupControllerListening {
    // Observe when a controller connects while the app is running
    [[NSNotificationCenter defaultCenter] addObserver:self
                                             selector:@selector(controllerDidConnect:)
                                                 name:GCControllerDidConnectNotification
                                               object:nil];

    // Observe when a controller disconnects
    [[NSNotificationCenter defaultCenter] addObserver:self
                                             selector:@selector(controllerDidDisconnect:)
                                                 name:GCControllerDidDisconnectNotification
                                               object:nil];
    
    NSTimer *timeoutTimer = [NSTimer timerWithTimeInterval:5.0
                                                   repeats:NO
                                                     block:^(NSTimer * _Nonnull timer) {
        NSLog(@"[GameController] 5-second discovery timeout reached. Stopping discovery and running engine.");
        [GCController stopWirelessControllerDiscovery];
        [self runIkemenGo];
    }];
    
    // Ensure timer fires even if UI/run loop mode shifts
    [[NSRunLoop currentRunLoop] addTimer:timeoutTimer forMode:NSRunLoopCommonModes];
}

- (void)controllerDidConnect:(NSNotification *)notification {
    GCController *controller = (GCController *)notification.object;
    NSLog(@"[GameController] CONNECTED: %@", controller.vendorName);
}

- (void)controllerDidDisconnect:(NSNotification *)notification {
    GCController *controller = (GCController *)notification.object;
    NSLog(@"[GameController] DISCONNECTED: %@", controller.vendorName);
}

- (UIInterfaceOrientationMask)supportedInterfaceOrientations {
    return IkemenOrientationIsLocked() ? IkemenLockedOrientationMask() : UIInterfaceOrientationMaskAll;
}

- (BOOL)shouldAutorotate {
    return !IkemenOrientationIsLocked();
}

- (BOOL)prefersHomeIndicatorAutoHidden {
    return YES;
}

// The game fills the screen, without the status bar.
- (BOOL)prefersStatusBarHidden {
    return YES;
}

- (UIRectEdge)preferredScreenEdgesDeferringSystemGestures {
    return UIRectEdgeAll;
}

- (void)viewDidLayoutSubviews {
    [super viewDidLayoutSubviews];
    for (UIView *view in self.view.subviews) {
        if ([view isKindOfClass:[MetalContainerView class]]) {
            self.metalContainerView = ((MetalContainerView*)view);
        }
        for (UIView *view2 in view.subviews) {
            if ([view2 isKindOfClass:[TouchOverlayView class]]) {
                self.overlayView = ((TouchOverlayView*)view2);
                break;
            }
        }
    }
}

- (void)viewWillTransitionToSize:(CGSize)size withTransitionCoordinator:(id<UIViewControllerTransitionCoordinator>)coordinator {
    [super viewWillTransitionToSize:size withTransitionCoordinator:coordinator];
    
    // I tried everything, damn this bug got hands
    CGFloat scale = self.view.window.screen.nativeScale;
    CGSize boundsSize = self.view.bounds.size;
    CGSize newDrawableSize = CGSizeMake(boundsSize.width * scale, boundsSize.height * scale);
    CGRect newFrame = CGRectMake(self.view.bounds.origin.x, self.view.bounds.origin.y, newDrawableSize.width, newDrawableSize.height);
    self.metalContainerView.frame = newFrame;
    self.metalContainerView.metalLayer.frame = newFrame;
    self.metalContainerView.metalLayer.contentsScale = scale;
    self.metalContainerView.metalLayer.drawableSize = newDrawableSize;
    
    [_metalContainerView setNeedsLayout];
    [_overlayView setNeedsLayout];
}

- (void)viewDidLoad {
    [super viewDidLoad];
    self.definesPresentationContext = YES;
    IkemenLockOrientationToCurrent(); // lock orientation to boot to band-aid controls and Metal view, damn this bug got hands
}

- (void)viewDidAppear:(BOOL)animated {
    [super viewDidAppear:animated];
    
    // Elevate the host window above SDL
    if (self.view.window) {
        self.view.window.windowLevel = UIWindowLevelNormal + 1.0f;
        if (@available(iOS 16.0, *)) {
            UIWindowScene *scene = self.view.window.windowScene;
            NSLog(@"[SceneGeometry] effectiveGeometry.interfaceOrientation=%d activationState=%ld",
                  (int)scene.effectiveGeometry.interfaceOrientation,
                  (long)scene.activationState);
        }
    }
    
    UIWindow *sdlWindow = IkemenGetSDLWindow();
    NSLog(@"[WindowLevels] host=%f sdl=%f hostIsKey=%d sdlIsKey=%d",
          self.view.window.windowLevel,
          sdlWindow.windowLevel,
          self.view.window.isKeyWindow,
          sdlWindow.isKeyWindow);

    // Don't start the engine again if it's already been
    if (self.ikemenStarted) {
        return;
    }
    
    // Get whether or not we should display the folder picker now.
    NSUserDefaults *defaults = [NSUserDefaults standardUserDefaults];
    BOOL shouldPickFolder = [defaults boolForKey:RestartFolderPickKey];
    BOOL didRestoreAssetsFolder = [self restoreAssetsFolder];

    // Must do this here (we shouldn't present the asset folder picker before the view is ready)
    if (didRestoreAssetsFolder && !shouldPickFolder) {
        [self prepareAssetsAndRun];
    } else {
        // Reset the value now
        if (shouldPickFolder) {
            [defaults setBool:NO forKey:RestartFolderPickKey];
            [defaults synchronize];
        }
        // NOW ask the user for the folder
        [self presentAssetsFolderPicker];
    }
}

- (BOOL)beginAccessingAssetsFolder {
    if (_securityScopeActive) {
        return YES;
    }
    
    self.securityScopeActive = [_assetsFolderURL startAccessingSecurityScopedResource];
    
    return _securityScopeActive;
}

- (BOOL)stopAccessingAssetsFolder {
    if (!_securityScopeActive) {
        return YES;
    }
    
    [_assetsFolderURL stopAccessingSecurityScopedResource];
    self.securityScopeActive = NO;
    
    return !_securityScopeActive;
}

- (NSURL *)assetInstallMarkerURL:(NSURL *)folderURL {
    return [folderURL URLByAppendingPathComponent:AssetsInstallMarkerName];
}

- (void)markAssetInstallationCompleteAtURL:(NSURL *)folderURL {
    NSURL *markerURL = [self assetInstallMarkerURL:folderURL];
    [CurrentAssetsVersion writeToURL:markerURL atomically:YES encoding:NSUTF8StringEncoding error:nil];
}

- (BOOL)isFirstAssetInstallationAtURL:(NSURL *)folderURL {
    if (!folderURL) {
        return YES;
    }
    return ![[NSFileManager defaultManager] fileExistsAtPath:[self assetInstallMarkerURL:folderURL].path];
}

// Determines if the manifest needs updating before checking external/script
- (BOOL)assetInstallationNeedsUpdateAtURL:(NSURL *)folderURL {
    NSURL *markerURL = [self assetInstallMarkerURL:folderURL];
    NSString *installedVersion = [NSString stringWithContentsOfURL:markerURL encoding:NSUTF8StringEncoding error:nil];
    return ![installedVersion isEqualToString:CurrentAssetsVersion];
}

- (void)presentAssetsFolderPicker {
    if (_ikemenStarted) {
        for (UIView *view in self.view.subviews) {
            for (UIView *view2 in view.subviews) {
                if ([view2 isKindOfClass:[TouchOverlayView class]]) {
                    self.overlayView = ((TouchOverlayView*)view2);
                }
            }
        }
        NSString *msg = NSLocalizedString(@"engineRestartMsg", @"Engine restart message");
        NSString *title = NSLocalizedString(@"engineRestartTitle", @"Engine restart title");
//        UIAlertController *restartAlert = [UIAlertController alertControllerWithTitle:title message:msg preferredStyle:UIAlertControllerStyleAlert];
        
        // Get the damn scene
        UIWindowScene *scene = self.view.window.windowScene;
        if (!scene) {
            for (UIScene *s in [UIApplication sharedApplication].connectedScenes) {
                if (s.activationState == UISceneActivationStateForegroundActive && [s isKindOfClass:[UIWindowScene class]]) {
                    scene = (UIWindowScene *)s;
                    break;
                }
            }
        }

        // Create a dedicated alert UIWindow elevated high above all SDL and host layers
        self.alertWindow = [[UIWindow alloc] initWithWindowScene:scene];
        self.alertWindow.windowLevel = UIWindowLevelAlert + 1000.0;
        
        UIViewController *alertVC = [[UIViewController alloc] init];
        alertVC.view.backgroundColor = [UIColor clearColor];
        self.alertWindow.rootViewController = alertVC;
        
        [self.alertWindow makeKeyAndVisible];
        
        // Create a custom modal view instead of UIAlertController to bypass GCD-based UIAlertAction dispatch. Tired of this crap.
        CGRect centerFrame = CGRectMake(self.alertWindow.frame.size.width/2 - 150, self.alertWindow.frame.size.height/2 - 100, 300, 200);
        UIView *customAlertView = [[UIView alloc] initWithFrame:centerFrame];
        customAlertView.backgroundColor = [UIColor colorWithRed:0.4 green:0.4 blue:0.4 alpha:0.8];
        customAlertView.layer.cornerRadius = 14.0;
        customAlertView.clipsToBounds = YES;
        
        UILabel *titleLabel = [[UILabel alloc] initWithFrame:CGRectMake(0, 0, 300, 45)];
        [titleLabel setTextAlignment:NSTextAlignmentCenter];
        [titleLabel setText:title];
        titleLabel.backgroundColor = [UIColor colorWithRed:0.25 green:0.25 blue:0.25 alpha:0.8];
        titleLabel.layer.maskedCorners = kCALayerMinXMinYCorner | kCALayerMaxXMinYCorner;
        titleLabel.layer.cornerRadius = 14.0;
        titleLabel.clipsToBounds = YES;
        
        UILabel *msgLabel = [[UILabel alloc] initWithFrame:CGRectMake(8, 8, 286, 200)];
        [msgLabel setLineBreakMode:NSLineBreakByWordWrapping];
        [msgLabel setNumberOfLines:5];
        [msgLabel setText:msg];
        
        UIButton *cancelButton = [UIButton buttonWithType:UIButtonTypeSystem];
        [cancelButton setTitle:NSLocalizedString(@"cancelStr", "Cancel command name") forState:UIControlStateNormal];
        cancelButton.backgroundColor = [UIColor colorWithRed:0.25 green:0.25 blue:0.25 alpha:0.8];
        cancelButton.frame = CGRectMake(0, 160, 150, 40);
        cancelButton.layer.maskedCorners = kCALayerMinXMaxYCorner;
        cancelButton.layer.cornerRadius = 14.0;
        cancelButton.clipsToBounds = YES;
        
        UIButton *exitButton = [UIButton buttonWithType:UIButtonTypeSystem];
        [exitButton setTitle:NSLocalizedString(@"exitStr", "Exit command name") forState:UIControlStateNormal];
        exitButton.backgroundColor = [UIColor colorWithRed:0.25 green:0.25 blue:0.25 alpha:0.8];
        exitButton.frame = CGRectMake(150, 160, 150, 40);
        exitButton.layer.maskedCorners = kCALayerMaxXMaxYCorner;
        exitButton.layer.cornerRadius = 14.0;
        exitButton.clipsToBounds = YES;
        
        // These will execute without GCD interference
        [exitButton addTarget:self action:@selector(handleExit:) forControlEvents:UIControlEventTouchUpInside];
        [cancelButton addTarget:self action:@selector(handleCancel:) forControlEvents:UIControlEventTouchUpInside];

        [alertVC.view addSubview:customAlertView];
        [customAlertView addSubview:titleLabel];
        [customAlertView addSubview:msgLabel];
        [customAlertView addSubview:exitButton];
        [customAlertView addSubview:cancelButton];
        
//        [restartAlert addAction:[UIAlertAction actionWithTitle:NSLocalizedString(@"cancelStr", @"Cancel command name") style:UIAlertActionStyleCancel handler:^(UIAlertAction *action) {
//            cleanupAlertWindow();
//            touchOverlaySubview.hidden = NO;
//            touchOverlaySubview.userInteractionEnabled = YES;
//            [touchOverlaySubview.window makeKeyAndVisible];
//            [touchOverlaySubview resetHideTimer];
//        }]];
//        [restartAlert addAction:[UIAlertAction actionWithTitle:NSLocalizedString(@"exitStr", @"Exit command name") style:UIAlertActionStyleDefault handler:^(UIAlertAction *action) {
//            cleanupAlertWindow();
//            NSUserDefaults *defaults = [NSUserDefaults standardUserDefaults];
//            [defaults setBool:YES forKey:RestartFolderPickKey];
//            [defaults synchronize];
//            [self stopIkemenEngine];
//        }]];
        
//        [alertVC presentViewController:restartAlert animated:NO completion:^{
//            NSLog(@"[SUCCESS] Alert completion fired cleanly on dedicated window!");
//        }];
//        UIViewController *topVC = self.view.window.rootViewController;
//        while (topVC.presentedViewController) {
//            topVC = topVC.presentedViewController;
//        }
//        [topVC presentViewController:restartAlert animated:NO completion:nil];
        return;
    } else {
        UIDocumentPickerViewController *picker = [[UIDocumentPickerViewController alloc] initForOpeningContentTypes:@[
            UTTypeFolder
        ]];
        picker.allowsMultipleSelection = NO;
        picker.delegate = self;
        [self presentViewController:picker animated:YES completion:nil];
    }
}

- (void)handleExit:(id)sender {
    NSUserDefaults *defaults = [NSUserDefaults standardUserDefaults];
    [defaults setBool:YES forKey:RestartFolderPickKey];
    [defaults synchronize];
    
    // Explicitly process or clean up on the current RunLoop
    CFRunLoopPerformBlock(CFRunLoopGetMain(), kCFRunLoopCommonModes, ^{
        self.alertWindow.hidden = YES;
        self.alertWindow = nil;
    });
    
    [self stopIkemenEngine];
}

- (void)handleCancel:(id)sender {
    if (self.overlayView) {
        self.overlayView.hidden = NO;
        self.overlayView.userInteractionEnabled = YES;
        [self.overlayView.window makeKeyAndVisible];
        [self.overlayView resetHideTimer];
    }
    
    // Explicitly process or clean up on the current RunLoop
    CFRunLoopPerformBlock(CFRunLoopGetMain(), kCFRunLoopCommonModes, ^{
        self.alertWindow.hidden = YES;
        self.alertWindow = nil;
    });
}

// User still didn't pick a folder? PICK FOR THEM.
- (void)useFallbackAssetsFolder {
    NSFileManager *fm = [NSFileManager defaultManager];
    NSURL *appSupport = [fm URLForDirectory:NSApplicationSupportDirectory
                                   inDomain:NSUserDomainMask
                          appropriateForURL:nil
                                     create:YES error:nil];
    NSURL *folderURL = [appSupport URLByAppendingPathComponent:@"assets" isDirectory:YES];
    NSError *error = nil;
    if (![fm createDirectoryAtURL:folderURL withIntermediateDirectories:YES attributes:nil error:&error]) {
        NSLog(@"Could not create fallback assets directory: %@", error);
        return;
    }
    self.assetsFolderURL = folderURL;
    self.securityScopeActive = NO;
}

- (void)runIkemenGo {
    if (_ikemenStarted || !_assetsFolderURL) {
        return;
    }

    const char *path =
        _assetsFolderURL.path.fileSystemRepresentation;

    if (!path) {
        NSLog(@"Assets folder has no filesystem path");
        return;
    }

    // argv[3] is the terminating NULL pointer.
    char **argv = calloc(4, sizeof(char *));
    if (!argv) {
        NSLog(@"Failed to allocate argv");
        return;
    }

    argv[0] = strdup("ikemen-go");
    argv[1] = strdup("--assets");
    argv[2] = strdup(path);
    argv[3] = NULL;

    if (!argv[0] || !argv[1] || !argv[2]) {
        for (int i = 0; i < 3; i++) {
            free(argv[i]);
        }
        free(argv);

        NSLog(@"Failed to allocate command-line arguments");
        return;
    }

    // Now start our SDL code
    self.ikemenStarted = YES;
    dispatch_async(dispatch_get_main_queue(), ^{
        SDL_SetMainReady();
        
        NSString *dbPath = [[NSURL fileURLWithPath:@"external/gamecontrollerdb.txt" relativeToURL:self.assetsFolderURL] path];
        if ([[NSFileManager defaultManager] fileExistsAtPath:dbPath]) {
            SDL_SetHint(SDL_HINT_GAMECONTROLLERCONFIG_FILE, [dbPath UTF8String]);
        }

        // ENABLE Apple's native controller subsystem (MFi)
        SDL_SetHint(SDL_HINT_JOYSTICK_MFI, "1");
        
        // Keep background events allowed
        SDL_SetHint(SDL_HINT_JOYSTICK_ALLOW_BACKGROUND_EVENTS, "1");

        // Force button labels mapping
        SDL_SetHint(SDL_HINT_GAMECONTROLLER_USE_BUTTON_LABELS, "1");

        // The game's commands for the touch controls (TouchControls.h)
        IkemenSetAppMessageHandler((void *)IkemenTouchControlsMessage);

        // Start SDL_main
        self.overlayView.sdlWasInit = YES;
        int result = SDL_main(3, argv);
        (void)result;

        self.ikemenStarted = NO;
        [self stopAccessingAssetsFolder];

        for (int i = 0; i < 3; i++) {
            free(argv[i]);
        }
        free(argv);
    });
}


- (void)prepareAssetsAndRun {
    BOOL isFirstRun = [self isFirstAssetInstallationAtURL:_assetsFolderURL];
    
    BOOL needsExtraction = isFirstRun || [self assetInstallationNeedsUpdateAtURL:_assetsFolderURL] || [self scriptFileDiffersFromBundleAtDestinationURL:_assetsFolderURL];
    
    // Check if the contents of the included external/script dir differ
    if (needsExtraction) {
        NSLog(@"Now (re?)-extracting included engine files...");
        NSString *msg;
        NSString *title;
        
        // Get the right dialog messages
        if (isFirstRun) {
            msg = NSLocalizedString(@"firstRunDialogMsg", @"First run dialog message");
            title = NSLocalizedString(@"firstRunDialogTitle", @"First run dialog title");
        } else {
            msg = NSLocalizedString(@"updatingFilesMsg", @"Update files dialog message");
            title = NSLocalizedString(@"updatingFilesTitle", @"Update files dialog title");
        }
        
        UIAlertController *loading = [UIAlertController alertControllerWithTitle:title message:msg preferredStyle:UIAlertControllerStyleAlert];
        UIActivityIndicatorView *spinner = [[UIActivityIndicatorView alloc] initWithActivityIndicatorStyle:UIActivityIndicatorViewStyleMedium];
        spinner.translatesAutoresizingMaskIntoConstraints = NO;
        [spinner startAnimating];
        [loading.view addSubview:spinner];
        [NSLayoutConstraint activateConstraints:@[
            [spinner.centerXAnchor constraintEqualToAnchor:loading.view.centerXAnchor],
            [spinner.bottomAnchor constraintEqualToAnchor:loading.view.bottomAnchor constant:-20]
        ]];
        [self presentViewController:loading animated:YES completion:nil];
        
        dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
            NSError *error = nil;
            BOOL success = [self extractZipAssetsToURL:self.assetsFolderURL error:&error];
            
            dispatch_async(dispatch_get_main_queue(), ^{
                [loading dismissViewControllerAnimated:YES completion:^{
                    if (!success) {
                        NSLog(@"Asset extraction failed: %@", error);
                        NSString *title = NSLocalizedString(@"extractionErrorTitle", @"Extraction failure dialog title");
                        UIAlertController *fail = [UIAlertController alertControllerWithTitle:title message:error.localizedDescription preferredStyle:UIAlertControllerStyleAlert];
                        
                        [fail addAction:[UIAlertAction actionWithTitle:NSLocalizedString(@"exitStr", @"Exit command name") style:UIAlertActionStyleDefault handler:nil]];
                        [self presentViewController:fail animated:YES completion:nil];
                        return;
                    }
                    
                    [self markAssetInstallationCompleteAtURL:self.assetsFolderURL];
                    [self setupControllerListening];
//                    [self runIkemenGo];
                }];
            });
        });
    } else {
        [self setupControllerListening];
//        [self runIkemenGo];
    }
}

- (void)documentPicker:(UIDocumentPickerViewController *)controller didPickDocumentsAtURLs:(NSArray<NSURL *> *)urls {
    NSURL *folderURL = urls.firstObject;
    if (![self saveBookmarkForURL:folderURL]) {
        [self useFallbackAssetsFolder];
    }
    
    [self prepareAssetsAndRun];
}

- (void)documentPickerWasCancelled:(UIDocumentPickerViewController *)controller {
    if (!self.assetsFolderURL) {
        [self useFallbackAssetsFolder];
    }
    [self prepareAssetsAndRun];
}

- (BOOL)crc32ForFileURL:(NSURL *)fileURL crc:(uint32_t *)outCRC size:(unsigned long long *)outSize error:(NSError **)error {
    NSFileHandle *file =
    [NSFileHandle fileHandleForReadingFromURL:fileURL
                                        error:error];
    
    if (file == nil) {
        return NO;
    }
    
    uint32_t crc = (uint32_t)crc32(0L, Z_NULL, 0);
    unsigned long long totalSize = 0;
    
    while (YES) {
        NSError *readError = nil;
        NSData *data = [file readDataUpToLength:1024 * 1024 error:&readError];
        
        if (readError != nil) {
            [file closeFile];
            
            if (error != NULL) {
                *error = readError;
            }
            
            return NO;
        }
        
        if (data.length == 0) {
            break;
        }
        
        crc = (uint32_t)crc32(crc, data.bytes, (uInt)data.length);
        
        totalSize += data.length;
    }
    
    [file closeFile];
    
    if (outCRC != NULL) {
        *outCRC = crc;
    }
    
    if (outSize != NULL) {
        *outSize = totalSize;
    }
    
    return YES;
}

- (BOOL)scriptFileDiffersFromBundleAtDestinationURL:(NSURL *)destinationURL {
    NSFileManager *fm = [NSFileManager defaultManager];
    
    // Get DefaultAssets.zip
    NSURL *zipURL = [[NSBundle mainBundle] URLForResource:@"DefaultAssets" withExtension:@"zip"];
    if (!zipURL) {
        NSLog(@"DefaultAssets.zip not found WTF YOU NEED THAT, STUPID ESE");
        return NO;
    }
    
    // Extract to tmp dir
    NSString *tmpPath = [NSTemporaryDirectory() stringByAppendingPathComponent:@"DefaultAssetsTmp"];
    [fm removeItemAtPath:tmpPath error:nil];
    
    BOOL didUnzip = [SSZipArchive unzipFileAtPath:zipURL.path toDestination:tmpPath];
    if (!didUnzip) {
        NSLog(@"DefaultAssets.zip not extractable. WTF did you do, holmes?!");
        return NO;
    }
    
    BOOL isDifferent = NO;
    NSURL *tmpScriptURL = [[NSURL fileURLWithPath:tmpPath] URLByAppendingPathComponent:@"external/script" isDirectory:YES];
    NSURL *dstScriptURL = [destinationURL URLByAppendingPathComponent:@"external/script" isDirectory:YES];
    
//    NSURL *tmpGCURL = [[NSURL fileURLWithPath:tmpPath] URLByAppendingPathComponent:@"external/gamecontrollerdb.txt" isDirectory:NO];
    NSURL *dstGCURL = [destinationURL URLByAppendingPathComponent:@"external/gamecontrollerdb.txt" isDirectory:NO];
    
    if (![fm fileExistsAtPath:dstScriptURL.path] || ![fm fileExistsAtPath:dstGCURL.path]) {
        isDifferent = YES;
    } else {
        NSDirectoryEnumerator<NSURL *> *enumerator = [fm enumeratorAtURL:tmpScriptURL includingPropertiesForKeys:@[NSURLIsDirectoryKey] options:0 errorHandler:nil];
        
        for (NSURL *tmpFileURL in enumerator) {
            NSNumber *fileIsDirectory = nil;
            
            if (![tmpFileURL getResourceValue:&fileIsDirectory forKey:NSURLIsDirectoryKey error:nil]) {
                continue;
            }
            
            if (fileIsDirectory.boolValue) {
                continue;
            }
            
            if (![[tmpFileURL.pathExtension lowercaseString] isEqualToString:@"lua"]) {
                continue;
            }
            
            NSString *relativePath = [tmpFileURL.path substringFromIndex:tmpScriptURL.path.length];
            
            if ([relativePath hasPrefix:@"/"]) {
                relativePath = [relativePath substringFromIndex:1];
            }
            
            NSURL *destinationFileURL = [dstScriptURL URLByAppendingPathComponent:relativePath];
            
            if (![fm fileExistsAtPath:destinationFileURL.path]) {
                NSLog(@"Missing extracted Lua file: %@", relativePath);
                isDifferent = YES;
                break;
            }
            
            uint32_t tmpCRC = 0;
            uint32_t dstCRC = 0;
            unsigned long long bundleSize = 0;
            unsigned long long destinationSize = 0;
            
            if (![self crc32ForFileURL:tmpFileURL crc:&tmpCRC size:&bundleSize error:nil]) {
                isDifferent = YES;
                break;
            }
            
            if (![self crc32ForFileURL:destinationFileURL crc:&dstCRC size:&destinationSize error:nil]) {
                isDifferent = YES;
                break;
            }
            
            if (bundleSize != destinationSize || tmpCRC != dstCRC) {
                NSLog(@"Changed Lua file: %@. Don't screw with it, K?", relativePath);
                isDifferent = YES;
                break;
            }
        }
    }
    
    // Remove the tmp dir now, we're done uhuhuhuhuhuhuh
    [fm removeItemAtPath:tmpPath error:nil];
    
    return isDifferent;
}

- (BOOL)extractZipAssetsToURL:(NSURL *)destinationFolderURL error:(NSError **)error {
    NSURL *zipURL = [[NSBundle mainBundle] URLForResource:@"DefaultAssets" withExtension:@"zip"];
    if (!zipURL) {
        if (error) {
            *error = [NSError errorWithDomain:@"IKEMENErrorDomain" code:404 userInfo:@{NSLocalizedDescriptionKey: @"DefaultAssets.zip not found in main bundle."}];
        }
        return NO;
    }
    
    [self beginAccessingAssetsFolder];
    
    @try {
        BOOL success = [SSZipArchive unzipFileAtPath:zipURL.path toDestination:destinationFolderURL.path];
        
        if (!success && error) {
            *error = [NSError errorWithDomain:@"IKEMENErrorDomain" code:500 userInfo:@{NSLocalizedDescriptionKey: @"SSZipArchive failed to extract DefaultAssets.zip"}];
        }
        return success;
    }
    @finally {
        
    }
}

- (BOOL)saveBookmarkForURL:(NSURL *)folderURL {
    // Don't leak security scope
    [self stopAccessingAssetsFolder];
    
    if (!folderURL) {
        return NO;
    }
    
    self.assetsFolderURL = folderURL;
    [self beginAccessingAssetsFolder];
    
    if (!_securityScopeActive) {
        NSLog(@"Could not access selected folder");
        return NO;
    }
    
    // Try to create the bookmark
    NSError *error = nil;
    NSData *bookmark = [folderURL bookmarkDataWithOptions:0 includingResourceValuesForKeys:nil relativeToURL:nil error:&error];
    
    if (!bookmark) {
        NSLog(@"Could not create bookmark: %@", error);
        [folderURL stopAccessingSecurityScopedResource];
        self.assetsFolderURL = nil;
        self.securityScopeActive = NO;
        return NO;
    }
    
    // Now save the bookmark
    [[NSUserDefaults standardUserDefaults] setObject:bookmark forKey:AssetsFolderBookmarkKey];
    return YES;
}

- (BOOL)restoreAssetsFolder {
    NSData *bookmark = [[NSUserDefaults standardUserDefaults] dataForKey:AssetsFolderBookmarkKey];
    
    if (!bookmark) {
        return NO;
    }
    
    BOOL isStale = NO;
    NSError *error = nil;
    
    NSURL *folderURL = [NSURL URLByResolvingBookmarkData:bookmark options:NSURLBookmarkResolutionWithoutUI relativeToURL:nil bookmarkDataIsStale:&isStale error:&error];
    
    if (!folderURL) {
        NSLog(@"Could not resolve assets bookmark: %@", error);
        return NO;
    }
    
    self.assetsFolderURL = folderURL;
    [self beginAccessingAssetsFolder];
    
    if (!_securityScopeActive) {
        NSLog(@"Could not reactivate folder access");
        return NO;
    }
    
    // Refresh the bookmark if iOS reports it's stale.
    if (isStale) {
        NSData *updatedBookmark = [folderURL bookmarkDataWithOptions:0 includingResourceValuesForKeys:nil relativeToURL:nil error:&error];
        if (updatedBookmark) {
            [[NSUserDefaults standardUserDefaults] setObject:updatedBookmark forKey:AssetsFolderBookmarkKey];
        }
    }
    
    return YES;
}

- (void)dealloc {
    if (self.securityScopeActive) {
        [self stopAccessingAssetsFolder];
    }
}

- (void)stopIkemenEngine {
    if (!_ikemenStarted) {
        return;
    }
    
    // Push SDL_QUIT event to cleanly break out of SDL_main
    SDL_Event event;
    event.type = SDL_QUIT;
    SDL_PushEvent(&event);
}

- (void)touchOverlayDidTapGameFolder:(TouchOverlayView*)view {
    [self presentAssetsFolderPicker];
}

- (void)prepareForSegue:(UIStoryboardSegue *)segue sender:(id)sender {
    if ([segue.destinationViewController isKindOfClass:[TouchOverlayViewController class]]) {
        TouchOverlayViewController *child = (TouchOverlayViewController*)segue.destinationViewController;
        [child setParentVC:self];
    }
}

@end
