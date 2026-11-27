//
//  TouchControls.m
//  I.K.E.M.E.N-Go
//
//  See TouchControls.h. Everything here runs on the main thread, as the game
//  does (SDL_main runs in a block on the main queue). So nothing here goes
//  through dispatch_async to the main queue: such a block would wait for the
//  game to quit. Notifications, timers and target-action all still arrive,
//  while SDL runs the run loop for its events.
//

#import "TouchControls.h"
#import "TouchLayoutEditor.h"
#import "SDLWindowFix.h"
#import <GameController/GameController.h>
#import <SDL2/SDL.h>
#import "libikemen.h"

static NSString * const ModeKey = @"IKEMENTouchControlsMode";
// The buttons the game last chose to show, for the next start.
static NSString * const ButtonsKey = @"IKEMENTouchButtons";
// And where on an arcade panel the attack buttons sit.
static NSString * const PanelKey = @"IKEMENTouchPanel";
static const int AllButtons = (1 << 10) - 1;

NSString *IkemenTouchGlyphsDirectory(void) {
    NSString *caches = NSSearchPathForDirectoriesInDomains(NSCachesDirectory, NSUserDomainMask, YES).firstObject;
    return [caches stringByAppendingPathComponent:@"touch-glyphs"];
}

static NSString *Str(NSString *key, NSString *english) {
    return NSLocalizedStringWithDefaultValue(key, nil, [NSBundle mainBundle], english, @"Touch controls");
}

@interface TouchControls ()
@property (nonatomic, weak) TouchOverlayView *overlay;
@property (nonatomic, strong) UIButton *settingsButton;
@property (nonatomic, strong) UIButton *keyboardButton;
@property (nonatomic, strong) UIView *settingsPanel;
@property (nonatomic, strong) NSArray<UIButton *> *modeButtons;
@property (nonatomic, strong) UIButton *editButton;
// The player's editor of the buttons' layout, while it is open.
@property (nonatomic, strong) TouchLayoutEditor *editor;
@property (nonatomic, strong) NSTimer *timer;
@property (nonatomic, assign) BOOL textEntry;
@property (nonatomic, assign) BOOL keyboardShown;
@property (nonatomic, assign) BOOL keyboardClosed;
@property (nonatomic, assign) int gameControllers;
// Whether the game has reported its controllers yet (IkemenMsgControllers).
@property (nonatomic, assign) BOOL gameReported;
- (void)handleCommand:(int)command param:(int)param;
@end

@implementation TouchControls

+ (instancetype)shared {
    static TouchControls *shared;
    if (!shared) {
        shared = [[TouchControls alloc] init];
    }
    return shared;
}

- (TouchControlsMode)mode {
    NSInteger m = [[NSUserDefaults standardUserDefaults] integerForKey:ModeKey];
    return (m >= TouchControlsModeAuto && m <= TouchControlsModeHide) ? (TouchControlsMode)m : TouchControlsModeAuto;
}

- (void)setMode:(TouchControlsMode)mode {
    [[NSUserDefaults standardUserDefaults] setInteger:mode forKey:ModeKey];
    [self update];
}

- (void)attachToOverlay:(TouchOverlayView *)overlay {
    if (self.overlay == overlay) {
        return;
    }
    // A new overlay, as for a new scene: the old one's timer, observers and
    // editor go.
    [self.editor cancel];
    self.editor = nil;
    [self.timer invalidate];
    [[NSNotificationCenter defaultCenter] removeObserver:self];
    self.overlay = overlay;

    self.settingsButton = [self roundButton:@"gearshape.fill" label:Str(@"touchSettingsStr", @"Touch controls") size:40 alpha:0.5];
    [self.settingsButton addTarget:self action:@selector(settingsTapped:) forControlEvents:UIControlEventTouchUpInside];
    self.keyboardButton = [self roundButton:@"keyboard" label:Str(@"keyboardStr", @"Keyboard") size:56 alpha:0.7];
    [self.keyboardButton addTarget:self action:@selector(keyboardTapped:) forControlEvents:UIControlEventTouchUpInside];
    self.keyboardButton.hidden = YES;
    [NSLayoutConstraint activateConstraints:@[
        // Clear of a notch or rounded corner.
        [self.settingsButton.topAnchor constraintEqualToAnchor:overlay.safeAreaLayoutGuide.topAnchor constant:8],
        [self.settingsButton.leadingAnchor constraintEqualToAnchor:overlay.safeAreaLayoutGuide.leadingAnchor constant:8],
        [self.keyboardButton.topAnchor constraintEqualToAnchor:overlay.safeAreaLayoutGuide.topAnchor constant:8],
        [self.keyboardButton.centerXAnchor constraintEqualToAnchor:overlay.centerXAnchor],
    ]];
    [self buildSettingsPanel];

    NSNumber *panel = [[NSUserDefaults standardUserDefaults] objectForKey:PanelKey];
    [overlay setPanelMap:panel ? (uint32_t)panel.intValue : TouchPanelDefaultMap];
    NSNumber *buttons = [[NSUserDefaults standardUserDefaults] objectForKey:ButtonsKey];
    [overlay setButtonsShown:buttons ? buttons.intValue : AllButtons];
    [overlay loadGlyphsFromDirectory:IkemenTouchGlyphsDirectory()];

    NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
    [nc addObserver:self selector:@selector(controllersChanged:) name:GCControllerDidConnectNotification object:nil];
    [nc addObserver:self selector:@selector(controllersChanged:) name:GCControllerDidDisconnectNotification object:nil];
    [nc addObserver:self selector:@selector(keyboardWillShow:) name:UIKeyboardWillShowNotification object:nil];
    [nc addObserver:self selector:@selector(keyboardWillHide:) name:UIKeyboardWillHideNotification object:nil];
    [nc addObserver:self selector:@selector(keyboardFrameWillChange:) name:UIKeyboardWillChangeFrameNotification object:nil];
    // The controller list again every second, in case a notification never
    // came (see the top of this file).
    self.timer = [NSTimer timerWithTimeInterval:1.0 target:self selector:@selector(tick:) userInfo:nil repeats:YES];
    [[NSRunLoop mainRunLoop] addTimer:self.timer forMode:NSRunLoopCommonModes];

    [self update];
}

// A round button with an icon at the top of the overlay, over the on-screen
// buttons, which takes its own touches (TouchOverlayView hitTest:).
- (UIButton *)roundButton:(NSString *)symbol label:(NSString *)label size:(CGFloat)size alpha:(CGFloat)alpha {
    UIButton *button = [UIButton buttonWithType:UIButtonTypeCustom];
    UIImageSymbolConfiguration *config = [UIImageSymbolConfiguration configurationWithPointSize:size * 0.45];
    [button setImage:[UIImage systemImageNamed:symbol withConfiguration:config] forState:UIControlStateNormal];
    button.tintColor = [UIColor whiteColor];
    button.backgroundColor = [UIColor colorWithWhite:0.15 alpha:0.8];
    button.layer.cornerRadius = size / 2.0;
    button.clipsToBounds = YES;
    button.alpha = alpha;
    button.accessibilityLabel = label;
    button.translatesAutoresizingMaskIntoConstraints = NO;
    [self.overlay addSubview:button];
    [NSLayoutConstraint activateConstraints:@[
        [button.widthAnchor constraintEqualToConstant:size],
        [button.heightAnchor constraintEqualToConstant:size],
    ]];
    return button;
}

// Automatic, Always show and Always hide, under the settings button. A
// panel of plain buttons rather than an alert or a menu, whose actions UIKit
// may hand to the main queue (see the top of this file).
- (void)buildSettingsPanel {
    UIStackView *panel = [[UIStackView alloc] init];
    panel.axis = UILayoutConstraintAxisVertical;
    panel.spacing = 1;
    panel.backgroundColor = [UIColor colorWithWhite:0.1 alpha:0.9];
    panel.layer.cornerRadius = 12;
    panel.clipsToBounds = YES;
    panel.translatesAutoresizingMaskIntoConstraints = NO;
    panel.hidden = YES;
    NSArray<NSString *> *titles = @[
        Str(@"touchAutoStr", @"Automatic"),
        Str(@"touchShowStr", @"Always show"),
        Str(@"touchHideStr", @"Always hide"),
    ];
    NSMutableArray<UIButton *> *buttons = [NSMutableArray array];
    for (NSUInteger i = 0; i < titles.count; i++) {
        UIButton *b = [UIButton buttonWithType:UIButtonTypeSystem];
        b.tag = (NSInteger)i;
        b.contentHorizontalAlignment = UIControlContentHorizontalAlignmentLeading;
        b.titleLabel.font = [UIFont systemFontOfSize:17];
        [b setTitleColor:[UIColor whiteColor] forState:UIControlStateNormal];
        b.backgroundColor = [UIColor colorWithWhite:0.2 alpha:0.9];
        [b addTarget:self action:@selector(modeTapped:) forControlEvents:UIControlEventTouchUpInside];
        [b.heightAnchor constraintEqualToConstant:44].active = YES;
        [panel addArrangedSubview:b];
        [buttons addObject:b];
    }
    self.modeButtons = buttons;
    // Then the layout editor.
    UIButton *edit = [UIButton buttonWithType:UIButtonTypeSystem];
    edit.contentHorizontalAlignment = UIControlContentHorizontalAlignmentLeading;
    edit.titleLabel.font = [UIFont systemFontOfSize:17];
    [edit setTitleColor:[UIColor whiteColor] forState:UIControlStateNormal];
    edit.backgroundColor = [UIColor colorWithWhite:0.2 alpha:0.9];
    [edit setTitle:[@"       " stringByAppendingString:Str(@"touchEditLayoutStr", @"Edit layout")] forState:UIControlStateNormal];
    [edit addTarget:self action:@selector(editTapped:) forControlEvents:UIControlEventTouchUpInside];
    [edit.heightAnchor constraintEqualToConstant:44].active = YES;
    [panel addArrangedSubview:edit];
    self.editButton = edit;
    [self.overlay addSubview:panel];
    [NSLayoutConstraint activateConstraints:@[
        [panel.topAnchor constraintEqualToAnchor:self.settingsButton.bottomAnchor constant:6],
        [panel.leadingAnchor constraintEqualToAnchor:self.settingsButton.leadingAnchor],
        [panel.widthAnchor constraintEqualToConstant:220],
    ]];
    self.settingsPanel = panel;
}

- (void)settingsTapped:(id)sender {
    self.settingsPanel.hidden = !self.settingsPanel.hidden;
    [self refreshModeButtons];
    [self.overlay resetHideTimer];
}

- (void)modeTapped:(UIButton *)sender {
    self.settingsPanel.hidden = YES;
    self.mode = (TouchControlsMode)sender.tag;
    [self.overlay resetHideTimer];
}

- (void)editTapped:(id)sender {
    self.settingsPanel.hidden = YES;
    [self openEditor];
}

- (BOOL)editing {
    return self.editor != nil;
}

- (void)openEditor {
    TouchOverlayView *overlay = self.overlay;
    if (self.editor || !overlay) {
        return;
    }
    self.settingsPanel.hidden = YES;
    overlay.editing = YES;
    __weak TouchControls *weakSelf = self;
    self.editor = [[TouchLayoutEditor alloc] initWithOverlay:overlay onClose:^{
        [weakSelf editorClosed];
    }];
    // The editor shows them whatever the setting.
    [self update];
    [overlay resetHideTimer];
}

- (void)editorClosed {
    self.editor = nil;
    self.overlay.editing = NO;
    [self update];
    [self.overlay resetHideTimer];
}

- (void)refreshModeButtons {
    TouchControlsMode mode = self.mode;
    NSArray<NSString *> *titles = @[
        Str(@"touchAutoStr", @"Automatic"),
        Str(@"touchShowStr", @"Always show"),
        Str(@"touchHideStr", @"Always hide"),
    ];
    for (UIButton *b in self.modeButtons) {
        NSString *mark = b.tag == mode ? @"✓ " : @"     ";
        [b setTitle:[@"  " stringByAppendingString:[mark stringByAppendingString:titles[(NSUInteger)b.tag]]] forState:UIControlStateNormal];
    }
}

// The keyboard button, shown while a text box is open and the keyboard
// closed: the game opens the keyboard again at its next frame.
- (void)keyboardTapped:(id)sender {
    [self makeSDLWindowKey];
    IkemenShowKeyboard();
}

// SDL's text field lives in SDL's window, under the app's (which shows the
// game and the buttons). The keyboard goes with the key window's first
// responder, so SDL's window is key while a text box is open.
- (void)makeSDLWindowKey {
    UIWindow *sdlWindow = IkemenGetSDLWindow();
    if (sdlWindow && !sdlWindow.isKeyWindow) {
        [sdlWindow makeKeyWindow];
    }
}

- (void)handleCommand:(int)command param:(int)param {
    switch (command) {
        case IkemenMsgTextEntry:
            self.textEntry = param != 0;
            if (self.textEntry) {
                // The game started SDL's text input just before; again, now
                // that its window is key.
                [self makeSDLWindowKey];
                SDL_StartTextInput();
            } else {
                UIWindow *host = self.overlay.window;
                if (host && !host.isKeyWindow) {
                    [host makeKeyWindow];
                }
            }
            [self update];
            break;
        case IkemenMsgTouchGlyphs:
            [self.overlay loadGlyphsFromDirectory:IkemenTouchGlyphsDirectory()];
            break;
        case IkemenMsgTouchButtons: {
            [[NSUserDefaults standardUserDefaults] setInteger:param forKey:ButtonsKey];
            int layout = self.overlay.layout;
            [self.overlay setButtonsShown:param];
            if (self.editor && self.overlay.layout != layout) {
                // The editor's changes were the old layout's, and the overlay
                // has the new one's own now.
                [self.editor discard];
            }
            break;
        }
        case IkemenMsgTouchPanel:
            [[NSUserDefaults standardUserDefaults] setInteger:param forKey:PanelKey];
            [self.overlay setPanelMap:(uint32_t)param];
            break;
        case IkemenMsgTouchEdit:
            [self openEditor];
            break;
        case IkemenMsgControllers:
            self.gameControllers = param;
            self.gameReported = YES;
            [self update];
            break;
    }
}

- (void)controllersChanged:(NSNotification *)notification {
    [self update];
}

- (void)tick:(NSTimer *)timer {
    [self update];
}

// Whether a real controller is connected: once the game runs, one the game
// has (it counts SDL's, leaving out the on-screen buttons' own); before, one
// the GameController framework knows. The framework's list may stand still
// while the game runs (see the top of this file), so it would keep the
// buttons hidden after a controller went.
- (BOOL)realControllerConnected {
    if (self.gameReported) {
        return self.gameControllers > 0;
    }
    return [GCController controllers].count > 0;
}

// Shows or hides each part, for the setting, the controllers connected, and
// whether a text box is open with its keyboard closed.
- (void)update {
    TouchOverlayView *overlay = self.overlay;
    if (!overlay) {
        return;
    }
    TouchControlsMode mode = self.mode;
    // The editor shows them whatever the setting.
    [overlay setControlsShown:self.editor != nil || mode == TouchControlsModeShow
                              || (mode == TouchControlsModeAuto && ![self realControllerConnected])];
    BOOL closed = self.textEntry && !self.keyboardShown;
    self.keyboardButton.hidden = !closed;
    if (closed && !self.keyboardClosed) {
        // A text box's keyboard just closed: the controls show, faded or
        // not, with the button that opens it again.
        [overlay resetHideTimer];
    }
    self.keyboardClosed = closed;
    if (!self.settingsPanel.hidden) {
        [self refreshModeButtons];
    }
}

#pragma mark - Keyboard

- (void)keyboardWillShow:(NSNotification *)notification {
    self.keyboardShown = YES;
    [self update];
}

- (void)keyboardWillHide:(NSNotification *)notification {
    self.keyboardShown = NO;
    UIView *view = self.overlay.window.rootViewController.view;
    IkemenSetKeyboardCover(0, (int)lround(view.bounds.size.height));
    [self update];
}

// Tells the game how much of its view the keyboard covers, which it then
// draws itself above (keyboardCover in the engine's src/text_entry.go). The
// game fills the app window's root view (MetalContainerView). A floating
// keyboard, on an iPad, covers nothing the game moves for.
- (void)keyboardFrameWillChange:(NSNotification *)notification {
    UIView *view = self.overlay.window.rootViewController.view;
    if (!view.window) {
        return;
    }
    CGRect end = [notification.userInfo[UIKeyboardFrameEndUserInfoKey] CGRectValue];
    UIScreen *screen = view.window.windowScene.screen;
    CGRect keyboard = screen ? [view convertRect:end fromCoordinateSpace:screen.coordinateSpace]
                             : [view convertRect:end fromView:nil];
    CGRect bounds = view.bounds;
    CGRect covered = CGRectIntersection(bounds, keyboard);
    CGFloat height = 0;
    if (!CGRectIsNull(covered) && !CGRectIsEmpty(covered) && CGRectGetMaxY(covered) >= CGRectGetMaxY(bounds) - 1.0) {
        height = CGRectGetMaxY(bounds) - CGRectGetMinY(covered);
    }
    IkemenSetKeyboardCover((int)lround(height), (int)lround(bounds.size.height));
}

@end

void IkemenTouchControlsMessage(int command, int param) {
    [[TouchControls shared] handleCommand:command param:param];
}
