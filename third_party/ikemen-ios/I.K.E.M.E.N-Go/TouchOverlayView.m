//
//  TouchOverlayView.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "TouchOverlayView.h"
#import "AnalogJoystickView.h"
#import "GamepadInputs.h"
#import "SDLWindowFix.h"
#import <Foundation/Foundation.h>
#import <QuartzCore/QuartzCore.h>
#import <SDL2/SDL.h>

// The controller the buttons play as. The game recognises it by its name
// (touchControlsName in the engine's src/system_sdl.go); without a vendor and
// product ID, SDL keeps that name.
static const char * const TouchControlsName = "IKEMEN Touch Controls";

// The buttons the game can hide, button i being bit i of the set it sends
// (touchButtonTokens in src/touch_glyphs.go): a, b, c, x, y, z, d, w, Start
// and Menu, by their tags in TouchOverlayView.xib. The first eight are the
// attack grid.
static const NSInteger ButtonTags[] = {1, 2, 3, 4, 5, 6, 9, 10, 7, 8};
// The names of their glyph images.
static NSString * const GlyphNames[] = {@"a", @"b", @"c", @"x", @"y", @"z", @"d", @"w", @"start", @"menu"};
static const int ButtonCount = 10;
static const int GridButtons = 8;
static const int StartIndex = 8;
static const int MenuIndex = 9;
static const int AllButtons = (1 << 10) - 1;
static const int LayoutGrid = 1 << 14;
// The rows of the arcade grid, right to left, as indexes above. A hidden
// button keeps its place, and the shown ones left of it move right over the
// gap (arrangeButtons).
static const int GridRows[2][4] = {{7, 5, 4, 3}, {6, 2, 1, 0}};
// The pressed and resting looks of a button, before the player's opacity.
static const CGFloat PressedAlpha = 0.3;
static const CGFloat RestingAlpha = 0.75;
// On a panel, Start and Menu at the top right: their size and spacing.
static const CGFloat CornerButtonSize = 55;
static const CGFloat CornerGap = 10;
static const CGFloat CornerMargin = 16;

// The views' own ways to let go (DPadView.m, AnalogJoystickView.m).
@interface DPadView (TouchOverlayRelease)
- (void)resetDPad;
@end
@interface AnalogJoystickView (TouchOverlayRelease)
- (void)resetHandle;
@end

@interface TouchOverlayView ()
@property (nonatomic, assign) GamepadButton activeButtons;
@property (nonatomic, strong) NSMutableSet<UIButton *> *currentlyPressedButtons;
@property (nonatomic, assign) CGSize lastKnownSize;
@property (nonatomic, assign, readwrite) BOOL controlsShown;
// The game's choice (setButtonsShown:): the buttons shown, the grid button
// whose place Start takes (-1 for none), and the grid layout.
@property (nonatomic, assign) int shownButtons;
@property (nonatomic, assign) int startSlot;
@property (nonatomic, assign) BOOL gridLayout;
@property (nonatomic, assign, readwrite) int layout;
@property (nonatomic, assign) uint32_t panelMap;
@property (nonatomic, strong) TouchCustomLayout *customLayout;
// Each button's glyph, over its background, and the title it has without one.
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, UIImageView *> *glyphViews;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, NSString *> *plainTitles;
@end

@implementation TouchOverlayView {
    // Each element's place in the layout (layoutPlace:centre:size:), in this
    // view's coordinates, before the player's changes.
    BOOL _placeKnown[TOUCH_EDIT_ELEMENTS];
    CGPoint _placeCentre[TOUCH_EDIT_ELEMENTS];
    CGFloat _placeSize[TOUCH_EDIT_ELEMENTS];
}

static int virtualJoystickIndex = -1;
static SDL_Joystick *virtualJoystick;
static SDL_JoystickID virtualJoystickID = -1;

- (void)setBounds:(CGRect)bounds {
    [super setBounds:bounds];
    NSLog(@"TouchOverlayView bounds changed to: %@", NSStringFromCGRect(bounds));
}

- (void)layoutSubviews {
    [super layoutSubviews];
    
    // Set ourselves as the control delegates (they should be created by here)
    _leftJoystickView.delegate = self;
    _rightJoystickView.delegate = self;
    _dPadView.delegate = self;
    
    self.vewlixButtonView.multipleTouchEnabled = YES;
    self.vewlixButtonView.userInteractionEnabled = YES;
    
    for (UIButton *btn in self.buttons) {
        btn.exclusiveTouch = NO;
        btn.userInteractionEnabled = NO;
        // A held button keeps its pressed look.
        btn.alpha = [self buttonAlpha:[self.currentlyPressedButtons containsObject:btn]];
    }
    
    // Reset timer and wake the overlay up when the device rotates
    if (!CGSizeEqualToSize(self.bounds.size, self.lastKnownSize)) {
        self.lastKnownSize = self.bounds.size;
        [self resetHideTimer];
    }

    [self applyVisibility];
    [self arrangeButtons];
}

- (instancetype)initWithCoder:(NSCoder *)coder {
    self = [super initWithCoder:coder];
    if (self) {
        [self commonInit];
    }
    return self;
}

- (void)awakeFromNib {
    [super awakeFromNib];
    
    if (self.leftJoystickView.delegate != nil || virtualJoystickIndex > -1) {
        return;
    }
    
    [self commonInit];
    [self setupViews];
    
    NSString *title = NSLocalizedString(@"gameFolderStr", @"Game folder selection button");
    [_gameFolderButton setTitle:title forState:UIControlStateNormal];
}

- (void)dealloc {
    [self detachVirtualJoystick];
}

- (instancetype)initWithFrame:(CGRect)frame {
    self = [super initWithFrame:frame];
    if (self) {
        [self commonInit];
        [self setupViews];
    }
    return self;
}

- (void)commonInit {
    _currentlyPressedButtons = [NSMutableSet set];
    _hitSlop = 12.0;
    _lastKnownSize = CGSizeZero;
    _controlsShown = YES;
    _shownButtons = AllButtons;
    _startSlot = -1;
    _gridLayout = NO;
    _layout = TouchLayoutClassic;
    _panelMap = TouchPanelDefaultMap;
    _customLayout = [TouchCustomLayout loadForLayout:_layout];
    _glyphViews = [NSMutableDictionary dictionary];
    _plainTitles = [NSMutableDictionary dictionary];
}

- (void)setupViews {
    if (virtualJoystickIndex > -1) {
        return;
    }
    
    self.multipleTouchEnabled = YES;
    self.userInteractionEnabled = YES;
    
    // Set the initial hide timer
    [self resetHideTimer];
}

- (void)resetHideTimer {
    // Cancel any previously scheduled hide perform request
    [NSObject cancelPreviousPerformRequestsWithTarget:self selector:@selector(hideOverlay) object:nil];
    self.alpha = 1.0;
    
    // Schedule hide overlay to run 5 seconds from now
    [self performSelector:@selector(hideOverlay) withObject:nil afterDelay:5.0];
}

- (void)hideOverlay {
    // The layout editor keeps them shown until the player closes it.
    if (self.editing) {
        return;
    }
#if TARGET_OS_SIMULATOR
    // Test hook, iOS Simulator builds only: test runs cannot touch the screen to
    // bring faded buttons back, and screenshot them at full opacity instead.
    if (getenv("IKEMEN_TEST_KEEP_TOUCH_CONTROLS") != NULL) {
        return;
    }
#endif
    // Fade out smoothly
    [UIView animateWithDuration:0.3 animations:^{
        self.alpha = 0.025f;
    } completion:nil];
}


#pragma mark - Game Folder button
- (IBAction)gameFolderButtonTapped:(id)sender {
    [self.delegate touchOverlayDidTapGameFolder:self];
}

#pragma mark - Multi-touch Behavior
- (UIView *)hitTest:(CGPoint)point withEvent:(UIEvent *)event {
    if (!self.userInteractionEnabled) {
        return nil;
    }

    if ([self pointInside:point withEvent:event]) {
        if (self.alpha < 1.0) {
            [self resetHideTimer];
        }

        UIView *hitView = [super hitTest:point withEvent:event];

        if (hitView && hitView != self && hitView != self.superview && hitView != self.vewlixButtonView && hitView != self.contentView) {
            return hitView;
        }
        
        // god this is hacky
        if (hitView == self.vewlixButtonView) {
            CGPoint pointInVewlix = [self convertPoint:point toView:self.vewlixButtonView];
            UIView *deeperHit = [self.vewlixButtonView hitTest:pointInVewlix withEvent:event];
            if (deeperHit && deeperHit != self.vewlixButtonView) {
                return deeperHit;
            }
        }
        return self;
    }
    
    return nil;
}

- (void)touchesBegan:(NSSet<UITouch *> *)touches withEvent:(nullable UIEvent *)event {
    [super touchesBegan:touches withEvent:event];
    [self processTouches:event.allTouches];
    [self resetHideTimer];
}

- (void)touchesMoved:(NSSet<UITouch *> *)touches withEvent:(nullable UIEvent *)event {
    [super touchesMoved:touches withEvent:event];
    [self processTouches:event.allTouches];
    [self resetHideTimer];
}

- (void)touchesEnded:(NSSet<UITouch *> *)touches withEvent:(nullable UIEvent *)event {
    [super touchesEnded:touches withEvent:event];
    [self processTouches:event.allTouches];
    [self resetHideTimer];
}

- (void)touchesCancelled:(NSSet<UITouch *> *)touches withEvent:(nullable UIEvent *)event {
    [super touchesCancelled:touches withEvent:event];
    [self processTouches:event.allTouches];
    [self resetHideTimer];
}

// This method processes touch events so that multiple button presses (A+B, X+Y, X+A, etc.) work
- (void)processTouches:(NSSet<UITouch *> *)touches {
    NSMutableSet<UIButton *> *newPressedButtons = [NSMutableSet set];
    
    // Evaluate active touch locations across the view. Hidden buttons take
    // none, and none do while the controls are hidden.
    for (UITouch *touch in touches) {
        if (!self.controlsShown || self.editing) {
            break;
        }
        if (touch.phase == UITouchPhaseBegan || touch.phase == UITouchPhaseMoved || touch.phase == UITouchPhaseStationary) {
            CGPoint loc = [touch locationInView:self];
            CGRect touchRect = CGRectMake(loc.x - _hitSlop, loc.y - _hitSlop, _hitSlop * 2.0, _hitSlop * 2.0);
            
            for (UIButton *button in self.buttons) {
                if (button.hidden) {
                    continue;
                }
                CGRect buttonFrameInSelf = [self convertRect:button.bounds fromView:button];
                CGRect expandedFrame = CGRectInset(buttonFrameInSelf, -self.hitSlop, -self.hitSlop);
                if (CGRectIntersectsRect(expandedFrame, touchRect)) {
                    [newPressedButtons addObject:button];
                }
            }
        }
    }
    
    // Evaluate transitions for EVERY button explicitly
    for (UIButton *btn in self.buttons) {
        BOOL wasPressed = [self.currentlyPressedButtons containsObject:btn];
        BOOL isPressed = [newPressedButtons containsObject:btn];

        if (isPressed && !wasPressed) {
            // Finger slid onto or tapped the button
            [self handleButtonState:btn isPressed:YES];
            [self.currentlyPressedButtons addObject:btn];
            
        } else if (!isPressed && wasPressed) {
            // Finger slid off the button or lifted
            [self handleButtonState:btn isPressed:NO];
            [self.currentlyPressedButtons removeObject:btn];
        }
        
        btn.alpha = [self buttonAlpha:isPressed];
    }
}

// A button's look, pressed or not, faded as the player chose.
- (CGFloat)buttonAlpha:(BOOL)pressed {
    return (pressed ? PressedAlpha : RestingAlpha) * self.customLayout.alpha;
}

- (void)initVirtualJoystick {
    if (virtualJoystickIndex < 0 && _sdlWasInit && self.controlsShown) {
        if (SDL_WasInit(SDL_INIT_JOYSTICK) == 0) {
            SDL_InitSubSystem(SDL_INIT_JOYSTICK);
        }
        
        // Force SDL event pump to ensure virtual device subsystem registers
        SDL_PumpEvents();

        // As SDL_JoystickAttachVirtual(SDL_JOYSTICK_TYPE_GAMECONTROLLER, 6, 15, 1),
        // with the name the game knows the touch controls by.
        SDL_VirtualJoystickDesc desc;
        SDL_zero(desc);
        desc.version = SDL_VIRTUAL_JOYSTICK_DESC_VERSION;
        desc.type = SDL_JOYSTICK_TYPE_GAMECONTROLLER;
        desc.naxes = 6;
        desc.nbuttons = 15;
        desc.nhats = 1;
        desc.name = TouchControlsName;
        virtualJoystickIndex = SDL_JoystickAttachVirtualEx(&desc);

        if (virtualJoystickIndex >= 0) {
            virtualJoystickID = SDL_JoystickGetDeviceInstanceID(virtualJoystickIndex);
            SDL_PumpEvents();
            virtualJoystick = SDL_JoystickOpen(virtualJoystickIndex);
        }
    }
}

// Removes the buttons' controller, if there is one: the game closes it, and a
// real controller moves into its slot (packControllers in src/system_sdl.go).
- (void)detachVirtualJoystick {
    if (virtualJoystickIndex < 0) {
        return;
    }
    if (SDL_WasInit(SDL_INIT_JOYSTICK) == 0) {
        // SDL quit, and took the joystick with it.
        virtualJoystick = NULL;
        virtualJoystickIndex = -1;
        virtualJoystickID = -1;
        return;
    }
    if (virtualJoystick != NULL) {
        SDL_JoystickClose(virtualJoystick);
        virtualJoystick = NULL;
    }
    // Its device index may have changed since it was attached.
    for (int i = 0; i < SDL_NumJoysticks(); i++) {
        if (SDL_JoystickGetDeviceInstanceID(i) == virtualJoystickID) {
            SDL_JoystickDetachVirtual(i);
            break;
        }
    }
    virtualJoystickIndex = -1;
    virtualJoystickID = -1;
}

#pragma mark - AnalogJoystickDelegate
- (void)joystick:(AnalogJoystickView *)joystick didMoveX:(CGFloat)x y:(CGFloat)y {
    if (self.editing) {
        // Held from before the editor opened: centred.
        x = 0;
        y = 0;
    }
    [self initVirtualJoystick];
    [self resetHideTimer];
    if (virtualJoystick == NULL) {
        return;
    }
    SInt16 sX = 0;
    SInt16 sY = 0;
    // Convert [-1.0,1.0] to [-32768,32767]
    if (x < 0) {
        sX = (SInt16)SDL_clamp((SInt16)(x * 32768.0), -32768, 32767);
    } else {
        sX = (SInt16)SDL_clamp((SInt16)(x * 32767.0), -32768, 32767);
    }
    if (y < 0) {
        sY = (SInt16)SDL_clamp((SInt16)(y * -32767.0), -32768, 32767);
    } else {
        sY = (SInt16)SDL_clamp((SInt16)(y * -32768.0), -32768, 32767);
    }
    // Handle stick movement
    SDL_JoystickSetVirtualAxis(virtualJoystick, (int)joystick.tag, sX);
    SDL_JoystickSetVirtualAxis(virtualJoystick, (int)(joystick.tag+1), sY);
}

#pragma mark - Buttons
- (IBAction)buttonTouchDown:(UIButton *)sender {
    [self handleButtonState:sender isPressed:YES];
}

- (IBAction)buttonTouchUp:(UIButton *)sender {
    [self handleButtonState:sender isPressed:NO];
}

- (void)handleButtonState:(UIButton * )sender isPressed:(BOOL)isPressed {
    [self initVirtualJoystick];
    [self resetHideTimer];
    if (virtualJoystick == NULL) {
        return;
    }
    GamepadButton buttonIndex = (1 << sender.tag);
    SDL_GameControllerButton sdlButton = SDL_CONTROLLER_BUTTON_INVALID;
    SDL_GameControllerAxis sdlAxis = SDL_CONTROLLER_AXIS_INVALID;
    
    switch (buttonIndex) {
        case GamepadButtonA:
            sdlButton = SDL_CONTROLLER_BUTTON_A;
            break;
        case GamepadButtonB:
            sdlButton = SDL_CONTROLLER_BUTTON_B;
            break;
        case GamepadButtonC:
            sdlAxis = SDL_CONTROLLER_AXIS_TRIGGERRIGHT;
            break;
        case GamepadButtonX:
            sdlButton = SDL_CONTROLLER_BUTTON_X;
            break;
        case GamepadButtonY:
            sdlButton = SDL_CONTROLLER_BUTTON_Y;
            break;
        case GamepadButtonZ:
            sdlButton = SDL_CONTROLLER_BUTTON_RIGHTSHOULDER;
            break;
        case GamepadButtonS:
            sdlButton = SDL_CONTROLLER_BUTTON_START;
            break;
        case GamepadButtonM:
            sdlButton = SDL_CONTROLLER_BUTTON_BACK;
            break;
        case GamepadButtonD:
            sdlButton = SDL_CONTROLLER_BUTTON_LEFTSHOULDER;
            break;
        case GamepadButtonW:
            sdlAxis = SDL_CONTROLLER_AXIS_TRIGGERLEFT;
            break;
        case GamepadButtonNone:
        default:
            sdlButton = SDL_CONTROLLER_BUTTON_INVALID;
            sdlAxis = SDL_CONTROLLER_AXIS_INVALID;
            break;
    }
    
    if (sdlButton >= 0) {
        SDL_JoystickSetVirtualButton(virtualJoystick, (int)sdlButton, isPressed ? SDL_PRESSED : SDL_RELEASED);
        SDL_JoystickUpdate();
    }
    if (sdlAxis >= 0) {
        SDL_JoystickSetVirtualAxis(virtualJoystick, sdlAxis, isPressed ? 32767 : 0);
        SDL_JoystickUpdate();
    }
}

#pragma mark - Shown buttons, layout and glyphs

- (UIButton *)buttonAtIndex:(int)i {
    for (UIButton *btn in self.buttons) {
        if (btn.tag == ButtonTags[i]) {
            return btn;
        }
    }
    return nil;
}

// Lets go of every button a finger holds.
- (void)releaseAllButtons {
    for (UIButton *btn in [self.currentlyPressedButtons allObjects]) {
        [self handleButtonState:btn isPressed:NO];
        btn.alpha = [self buttonAlpha:NO];
    }
    [self.currentlyPressedButtons removeAllObjects];
}

- (void)setControlsShown:(BOOL)shown {
    if (shown == _controlsShown) {
        return;
    }
    _controlsShown = shown;
    if (!shown) {
        [self releaseAllButtons];
        [self detachVirtualJoystick];
    }
    [self applyVisibility];
    [self setNeedsLayout];
}

- (void)setButtonsShown:(int)message {
    int slot = ((message >> 10) & 0xF) - 1;
    // The layout, or from a game that sends none, the grid bit.
    int layout = (int)(((uint32_t)message >> 16) & 0xF);
    if (layout == 0) {
        layout = (message & LayoutGrid) != 0 ? TouchLayoutGrid : TouchLayoutClassic;
    } else if (layout > TouchLayoutStaggered) {
        layout = TouchLayoutClassic;
    }
    BOOL grid = layout == TouchLayoutGrid;
    int set = message & AllButtons;
    // Start goes into the grid only over a hidden grid button, and only if it
    // shows itself.
    if (slot >= GridButtons || (slot >= 0 && (set & (1 << slot)) != 0) || (set & (1 << StartIndex)) == 0) {
        slot = -1;
    }
    if (set == _shownButtons && slot == _startSlot && layout == _layout) {
        return;
    }
    _shownButtons = set;
    _startSlot = slot;
    _gridLayout = grid;
    if (layout != _layout) {
        _layout = layout;
        // Each layout keeps the player's own changes, its opacity too.
        self.customLayout = [TouchCustomLayout loadForLayout:layout];
        for (UIButton *btn in self.buttons) {
            btn.alpha = [self buttonAlpha:[self.currentlyPressedButtons containsObject:btn]];
        }
    }
    for (int i = 0; i < ButtonCount; i++) {
        UIButton *btn = [self buttonAtIndex:i];
        if (btn && (set & (1 << i)) == 0 && [self.currentlyPressedButtons containsObject:btn]) {
            [self handleButtonState:btn isPressed:NO];
            [self.currentlyPressedButtons removeObject:btn];
            btn.alpha = [self buttonAlpha:NO];
        }
    }
    [self applyVisibility];
    [self arrangeButtons];
    [self setNeedsLayout];
}

// Shows or hides each part, for controlsShown and the buttons the game chose.
- (void)applyVisibility {
    BOOL shown = self.controlsShown;
    // The analog sticks sit where a panel's buttons go.
    BOOL panel = TouchLayoutIsPanel(self.layout);
    self.dPadView.hidden = !shown;
    self.leftJoystickView.hidden = !shown || panel;
    self.rightJoystickView.hidden = panel;
    self.vewlixButtonView.hidden = !shown;
    // The player's opacity, on what the editor moves as on Android: the
    // d-pad and the buttons (with their pressed look), not the sticks.
    self.dPadView.alpha = self.customLayout.alpha;
    for (int i = 0; i < ButtonCount; i++) {
        [self buttonAtIndex:i].hidden = !shown || (self.shownButtons & (1 << i)) == 0;
    }
}

// Moves the button v, by a transform (Auto Layout and the xib's frames stay
// as they are), so that its centre is at p, in vewlixButtonView's
// coordinates.
- (void)moveButton:(UIView *)v toGridPoint:(CGPoint)p {
    CGPoint target = [self.vewlixButtonView convertPoint:p toView:v.superview];
    v.transform = CGAffineTransformMakeTranslation(target.x - v.center.x, target.y - v.center.y);
}

// Element i of TouchEditNames: the d-pad, then buttons 0 to 9.
- (UIView *)elementView:(int)i {
    if (i == 0) {
        return self.dPadView;
    }
    return i < TOUCH_EDIT_ELEMENTS ? [self buttonAtIndex:i - 1] : nil;
}

// v's centre in this view's coordinates as the xib places it, without its
// transform.
- (CGPoint)naturalCentre:(UIView *)v {
    return [v.superview convertPoint:v.center toView:self];
}

- (void)setPlace:(int)i centre:(CGPoint)centre size:(CGFloat)size {
    _placeKnown[i] = YES;
    _placeCentre[i] = centre;
    _placeSize[i] = size;
}

// Lays everything out, as the Android app does (third_party/ikemen-droid's
// ControllerOverlay applyLayout): first where the layout puts each element
// (arrangeGrid or placePanel), then where the player moved and resized it
// (customLayout), by a transform from its place in the xib (Auto Layout and
// the xib's frames stay as they are).
- (void)arrangeButtons {
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        [self elementView:i].transform = CGAffineTransformIdentity;
        _placeKnown[i] = NO;
    }
    for (int i = 0; i < GridButtons; i++) {
        if (![self buttonAtIndex:i]) {
            return; // not loaded yet
        }
    }
    // The buttons' own places first: Auto Layout reaches the xib's views
    // after this one's layoutSubviews.
    [self.vewlixButtonView.superview layoutIfNeeded];
    if (self.bounds.size.width <= 0 || self.bounds.size.height <= 0) {
        return;
    }
    if (TouchLayoutIsPanel(self.layout)) {
        [self placePanel];
    } else {
        [self arrangeGrid];
    }
    // The places the layout gave by moving the views, and those it left.
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        UIView *v = [self elementView:i];
        if (!v || _placeKnown[i]) {
            continue;
        }
        CGPoint c = [self naturalCentre:v];
        [self setPlace:i centre:CGPointMake(c.x + v.transform.tx, c.y + v.transform.ty) size:v.bounds.size.width];
    }
    [self applyCustom];
}

// Moves and scales each element to its place, or the player's.
- (void)applyCustom {
    CGSize size = self.bounds.size;
    TouchCustomLayout *custom = self.customLayout;
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        UIView *v = [self elementView:i];
        if (!v || !_placeKnown[i]) {
            continue;
        }
        CGPoint c = _placeCentre[i];
        CGFloat diameter = _placeSize[i];
        CGFloat width = v.bounds.size.width;
        CGFloat scale = width > 0 ? diameter / width : 1.0;
        if ([custom hasPosition:i]) {
            CGPoint own = [custom centreOf:i];
            diameter *= [custom sizeOf:i];
            scale = width > 0 ? diameter / width : 1.0;
            // Wherever the player put it, or a saved place puts it on this
            // screen, it stays whole inside the safe area.
            c = [self clampCentre:CGPointMake(own.x * size.width, own.y * size.height)
                       halfWidth:width * scale / 2.0
                      halfHeight:v.bounds.size.height * scale / 2.0];
        }
        CGPoint natural = [self naturalCentre:v];
        v.transform = CGAffineTransformScale(CGAffineTransformMakeTranslation(c.x - natural.x, c.y - natural.y), scale, scale);
    }
}

// c moved as little as it takes for a rectangle reaching hw and hh from it
// to stay inside the safe area; centred in it where that is too small. As
// the Android app's ControllerOverlay clampCentre.
- (CGPoint)clampCentre:(CGPoint)c halfWidth:(CGFloat)hw halfHeight:(CGFloat)hh {
    CGRect area = UIEdgeInsetsInsetRect(self.bounds, self.safeAreaInsets);
    CGFloat l = CGRectGetMinX(area) + hw, r = CGRectGetMaxX(area) - hw;
    CGFloat t = CGRectGetMinY(area) + hh, b = CGRectGetMaxY(area) - hh;
    return CGPointMake(l > r ? (l + r) / 2.0 : MIN(MAX(c.x, l), r),
                       t > b ? (t + b) / 2.0 : MIN(MAX(c.y, t), b));
}

// An arcade panel (TouchPanelPlace) in the safe area: the d-pad where the
// stick is, each attack button in its place on the panel (panelMap), Start
// in its hidden button's place or, with Menu, in the top right corner.
- (void)placePanel {
    CGRect area = UIEdgeInsetsInsetRect(self.bounds, self.safeAreaInsets);
    TouchPanelPlacement placement = TouchPanelPlace(self.layout, area);
    [self setPlace:0 centre:placement.dpadCentre size:placement.dpad];
    BOOL startPlaced = NO;
    for (int b = 0; b < GridButtons; b++) {
        int p = TouchPanelPlaceOf(self.panelMap, b);
        if (p < 0) {
            continue;
        }
        [self setPlace:1 + b centre:placement.centre[p] size:placement.button];
        if (b == self.startSlot) {
            [self setPlace:1 + StartIndex centre:placement.centre[p] size:placement.button];
            startPlaced = YES;
        }
    }
    CGFloat x = CGRectGetMaxX(area) - CornerMargin - CornerButtonSize / 2.0;
    CGFloat y = CGRectGetMinY(area) + CornerMargin + CornerButtonSize / 2.0;
    // Below the game folder button, which shares that corner.
    UIButton *folder = self.gameFolderButton;
    if (folder && !folder.hidden && folder.superview) {
        CGRect f = [self convertRect:folder.bounds fromView:folder];
        if (CGRectGetMinX(f) < x + CornerButtonSize / 2.0) {
            y = MAX(y, CGRectGetMaxY(f) + CornerGap + CornerButtonSize / 2.0);
        }
    }
    if (!startPlaced) {
        [self setPlace:1 + StartIndex centre:CGPointMake(x, y) size:CornerButtonSize];
        x -= CornerButtonSize + CornerGap;
    }
    [self setPlace:1 + MenuIndex centre:CGPointMake(x, y) size:CornerButtonSize];
}

// The classic and grid layouts: in the classic one each row's shown buttons
// keep their order in the row's rightmost places, Start taking the place of
// the grid button start.slot names; in the grid layout each punch sits
// straight above its kick, with Start, in the grid, apart on the right. Start
// and Menu close up when one of them leaves.
- (void)arrangeGrid {
    UIView *start = [self buttonAtIndex:StartIndex];
    UIView *menu = [self buttonAtIndex:MenuIndex];
    int slot = start ? self.startSlot : -1;

    if (self.gridLayout) {
        CGFloat size = [self buttonAtIndex:0].bounds.size.width;
        CGFloat gap = 8.0;
        CGFloat top = CGFLOAT_MAX;
        for (int i = 0; i < GridButtons; i++) {
            UIView *b = [self buttonAtIndex:i];
            top = MIN(top, b.center.y - b.bounds.size.height / 2.0);
        }
        CGFloat topY = top + size / 2.0, bottomY = topY + size + gap;
        CGFloat x = self.vewlixButtonView.bounds.size.width - size / 2.0;
        if (slot >= 0) {
            [self moveButton:start toGridPoint:CGPointMake(x, (topY + bottomY) / 2.0)];
            x -= size + 20.0;
        }
        // Columns from the right: w over d (while either shows), z over c,
        // y over b, x over a.
        BOOL dw = (self.shownButtons & (3 << 6)) != 0;
        const int columns[4][2] = {{7, 6}, {5, 2}, {4, 1}, {3, 0}};
        for (int c = 0; c < 4; c++) {
            if (c == 0 && !dw) {
                continue;
            }
            [self moveButton:[self buttonAtIndex:columns[c][0]] toGridPoint:CGPointMake(x, topY)];
            [self moveButton:[self buttonAtIndex:columns[c][1]] toGridPoint:CGPointMake(x, bottomY)];
            x -= size + gap;
        }
    } else {
        for (int r = 0; r < 2; r++) {
            int place = 0;
            for (int k = 0; k < 4; k++) {
                int i = GridRows[r][k];
                UIView *v = i == slot ? start : [self buttonAtIndex:i];
                if (v.hidden) {
                    continue;
                }
                UIView *spot = [self buttonAtIndex:GridRows[r][place++]];
                [self moveButton:v toGridPoint:spot.center];
            }
        }
    }

    // Start and Menu sit side by side: one alone moves to the middle.
    if (start && menu) {
        BOOL startHere = !start.hidden && slot < 0;
        if (startHere != !menu.hidden) {
            UIView *alone = startHere ? start : menu;
            CGFloat mid = (start.center.x + menu.center.x) / 2.0;
            alone.transform = CGAffineTransformMakeTranslation(mid - alone.center.x, 0);
        }
    }
}

- (void)loadGlyphsFromDirectory:(NSString *)dir {
    for (int i = 0; i < ButtonCount; i++) {
        UIButton *btn = [self buttonAtIndex:i];
        if (!btn) {
            continue;
        }
        NSNumber *key = @(i);
        if (!self.plainTitles[key]) {
            self.plainTitles[key] = [btn titleForState:UIControlStateNormal] ?: @"";
        }
        UIImageView *glyph = self.glyphViews[key];
        if (!glyph) {
            // A fifth of the way in, its pixels kept sharp as the game draws them.
            CGFloat inset = MIN(btn.bounds.size.width, btn.bounds.size.height) * 0.2;
            glyph = [[UIImageView alloc] initWithFrame:CGRectInset(btn.bounds, inset, inset)];
            glyph.autoresizingMask = UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
            glyph.contentMode = UIViewContentModeScaleAspectFit;
            glyph.userInteractionEnabled = NO;
            glyph.layer.magnificationFilter = kCAFilterNearest;
            glyph.layer.minificationFilter = kCAFilterNearest;
            [btn addSubview:glyph];
            self.glyphViews[key] = glyph;
        }
        NSString *path = [dir stringByAppendingPathComponent:[GlyphNames[i] stringByAppendingString:@".png"]];
        // Read afresh: the game rewrites the files under the same names.
        NSData *data = [NSData dataWithContentsOfFile:path];
        UIImage *image = data ? [UIImage imageWithData:data] : nil;
        glyph.image = image;
        glyph.hidden = image == nil;
        [btn setTitle:image ? @"" : self.plainTitles[key] forState:UIControlStateNormal];
    }
}

#pragma mark - Panels and the player's layout

- (void)setPanelMap:(uint32_t)map {
    if (map == 0) {
        map = TouchPanelDefaultMap;
    }
    if (map == _panelMap) {
        return;
    }
    _panelMap = map;
    [self arrangeButtons];
    [self setNeedsLayout];
}

- (TouchCustomLayout *)custom {
    return self.customLayout;
}

- (void)setCustom:(TouchCustomLayout *)custom {
    self.customLayout = custom ?: [[TouchCustomLayout alloc] init];
    for (UIButton *btn in self.buttons) {
        btn.alpha = [self buttonAlpha:[self.currentlyPressedButtons containsObject:btn]];
    }
    [self applyVisibility];
    [self arrangeButtons];
}

- (void)saveCustom {
    [self.customLayout saveForLayout:self.layout];
}

- (void)setEditing:(BOOL)editing {
    _editing = editing;
    [self releaseAllButtons];
    // A finger already on the d-pad or a stick keeps reaching it: centred,
    // and kept so while editing (dPad:didChangeDirection:, joystick:didMoveX:y:).
    // Without their controller yet, nothing was ever held.
    if (virtualJoystick != NULL) {
        [self.dPadView resetDPad];
        [self.leftJoystickView resetHandle];
        [self.rightJoystickView resetHandle];
    }
    [self resetHideTimer];
}

// Whether v shows: neither it nor a view it is in, up to this one, hidden.
- (BOOL)isShown:(UIView *)v {
    for (UIView *p = v; p && p != self; p = p.superview) {
        if (p.hidden) {
            return NO;
        }
    }
    return v != nil;
}

- (CGRect)elementRect:(int)i {
    UIView *v = [self elementView:i];
    if (![self isShown:v]) {
        return CGRectNull;
    }
    // Takes its transform: where it is drawn.
    return [self convertRect:v.bounds fromView:v];
}

- (int)elementAtPoint:(CGPoint)point {
    int best = -1;
    CGFloat bestDist = CGFLOAT_MAX;
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        CGRect r = [self elementRect:i];
        if (CGRectIsNull(r) || !CGRectContainsPoint(r, point)) {
            continue;
        }
        CGFloat dx = CGRectGetMidX(r) - point.x, dy = CGRectGetMidY(r) - point.y;
        CGFloat d = dx * dx + dy * dy;
        if (d < bestDist) {
            best = i;
            bestDist = d;
        }
    }
    return best;
}

- (BOOL)layoutPlace:(int)i centre:(CGPoint *)centre size:(CGFloat *)size {
    if (i < 0 || i >= TOUCH_EDIT_ELEMENTS || !_placeKnown[i]) {
        return NO;
    }
    if (centre) {
        *centre = _placeCentre[i];
    }
    if (size) {
        *size = _placeSize[i];
    }
    return YES;
}

#pragma mark - DPadView Delegate
- (void)dPad:(DPadView *)dPad didChangeDirection:(DPadDirection)direction {
    if (self.editing) {
        // Held from before the editor opened: centred.
        direction = DPadDirectionNeutral;
    }
    [self initVirtualJoystick];
    if (virtualJoystick == NULL) {
        return;
    }

    // Dispatch as POV Hat 0 (Primary mapping for I.K.E.M.E.N-Go joystick reader)
    Uint8 hatValue = SDL_HAT_CENTERED;
    if (direction & DPadDirectionUp)    hatValue |= SDL_HAT_UP;
    if (direction & DPadDirectionDown)  hatValue |= SDL_HAT_DOWN;
    if (direction & DPadDirectionLeft)  hatValue |= SDL_HAT_LEFT;
    if (direction & DPadDirectionRight) hatValue |= SDL_HAT_RIGHT;
    
    SDL_JoystickSetVirtualHat(virtualJoystick, 0, hatValue);

    // Dispatch as GameController DPad Buttons (Fallback mapping)
    SDL_JoystickSetVirtualButton(virtualJoystick, SDL_CONTROLLER_BUTTON_DPAD_UP,
                                 (direction & DPadDirectionUp) ? SDL_PRESSED : SDL_RELEASED);
    SDL_JoystickSetVirtualButton(virtualJoystick, SDL_CONTROLLER_BUTTON_DPAD_DOWN,
                                 (direction & DPadDirectionDown) ? SDL_PRESSED : SDL_RELEASED);
    SDL_JoystickSetVirtualButton(virtualJoystick, SDL_CONTROLLER_BUTTON_DPAD_LEFT,
                                 (direction & DPadDirectionLeft) ? SDL_PRESSED : SDL_RELEASED);
    SDL_JoystickSetVirtualButton(virtualJoystick, SDL_CONTROLLER_BUTTON_DPAD_RIGHT,
                                 (direction & DPadDirectionRight) ? SDL_PRESSED : SDL_RELEASED);
}

@end
