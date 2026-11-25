//
//  TouchControls.h
//  I.K.E.M.E.N-Go
//
//  Decides whether the on-screen buttons show, from the player's setting
//  (automatic, always shown or always hidden) and the controllers connected,
//  and keeps the settings and keyboard buttons that go with them, as the
//  Android app's TouchControls does. The game tells it when a text box opens
//  or closes, when it wrote the buttons' glyph images, which buttons to show
//  and how many real controllers it has, through IkemenTouchControlsMessage.
//

#import <UIKit/UIKit.h>
#import "TouchOverlayView.h"

// The game's commands (the engine's src/app_messages.go).
enum {
    IkemenMsgTextEntry    = 0x8001, // a text box opened (1) or closed (0)
    IkemenMsgTouchGlyphs  = 0x8002, // the buttons' glyph images changed
    IkemenMsgTouchButtons = 0x8003, // the buttons to show (TouchOverlayView setButtonsShown:)
    IkemenMsgControllers  = 0x8004, // the number of real controllers the game has
};

typedef NS_ENUM(NSInteger, TouchControlsMode) {
    TouchControlsModeAuto = 0, // hidden while a real controller is connected
    TouchControlsModeShow = 1,
    TouchControlsModeHide = 2,
};

@interface TouchControls : NSObject

+ (instancetype)shared;

// Takes charge of overlay, once its xib's views are in place.
- (void)attachToOverlay:(TouchOverlayView *)overlay;

@property (nonatomic, assign) TouchControlsMode mode;

@end

// The function the app gives the game (IkemenSetAppMessageHandler) for its
// commands. The game calls it on the main thread.
void IkemenTouchControlsMessage(int command, int param);

// Where the game writes the buttons' glyph images: Library/Caches/touch-glyphs
// (touchGlyphsDir in the engine's src/app_ios.go).
NSString *IkemenTouchGlyphsDirectory(void);
