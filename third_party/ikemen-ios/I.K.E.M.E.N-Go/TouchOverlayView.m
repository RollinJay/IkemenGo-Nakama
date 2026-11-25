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
#import <SDL2/SDL.h>

@interface TouchOverlayView ()
@property (nonatomic, assign) GamepadButton activeButtons;
@property (nonatomic, strong) NSMutableSet<UIButton *> *currentlyPressedButtons;
@property (nonatomic, assign) CGSize lastKnownSize;
@end

@implementation TouchOverlayView

static int virtualJoystickIndex = -1;
static SDL_Joystick *virtualJoystick;

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
        btn.alpha = 0.75f;
    }
    
    // Reset timer and wake the overlay up when the device rotates
    if (!CGSizeEqualToSize(self.bounds.size, self.lastKnownSize)) {
        self.lastKnownSize = self.bounds.size;
        [self resetHideTimer];
    }
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
    if (virtualJoystick != NULL) {
        SDL_JoystickClose(virtualJoystick);
        virtualJoystick = NULL;
    }
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
    
    // Evaluate active touch locations across the view
    for (UITouch *touch in touches) {
        if (touch.phase == UITouchPhaseBegan || touch.phase == UITouchPhaseMoved || touch.phase == UITouchPhaseStationary) {
            CGPoint loc = [touch locationInView:self];
            CGRect touchRect = CGRectMake(loc.x - _hitSlop, loc.y - _hitSlop, _hitSlop * 2.0, _hitSlop * 2.0);
            
            for (UIButton *button in self.buttons) {
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
        
        btn.alpha = isPressed ? 0.3f : 0.75f;
    }
}

- (void)initVirtualJoystick {
    if (virtualJoystickIndex < 0 && _sdlWasInit) {
        if (SDL_WasInit(SDL_INIT_JOYSTICK) == 0) {
            SDL_InitSubSystem(SDL_INIT_JOYSTICK);
        }
        
        // Force SDL event pump to ensure virtual device subsystem registers
        SDL_PumpEvents();

        virtualJoystickIndex = SDL_JoystickAttachVirtual(SDL_JOYSTICK_TYPE_GAMECONTROLLER, 6, 15, 1);

        if (virtualJoystickIndex >= 0) {
            SDL_PumpEvents();
            virtualJoystick = SDL_JoystickOpen(virtualJoystickIndex);
        }
    }
}

#pragma mark - AnalogJoystickDelegate
- (void)joystick:(AnalogJoystickView *)joystick didMoveX:(CGFloat)x y:(CGFloat)y {
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

#pragma mark - DPadView Delegate
- (void)dPad:(DPadView *)dPad didChangeDirection:(DPadDirection)direction {
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
