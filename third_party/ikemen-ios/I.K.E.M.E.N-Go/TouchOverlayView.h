//
//  TouchOverlayView.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "AnalogJoystickView.h"
#import "DPadView.h"
#import "TouchPanels.h"
#import <UIKit/UIKit.h>

@class TouchOverlayView;

@protocol GameFolderButtonDelegate <NSObject>
- (void)touchOverlayDidTapGameFolder:(TouchOverlayView *)view;
@end

@interface TouchOverlayView : UIView <AnalogJoystickDelegate, DPadDelegate, UIGestureRecognizerDelegate>

@property (nonatomic, weak) id<GameFolderButtonDelegate> delegate;
@property (nonatomic, strong) IBOutlet UIButton *gameFolderButton;
@property (nonatomic, strong) IBOutlet UIView *vewlixButtonView;
@property (nonatomic, strong) IBOutlet DPadView *dPadView;
@property (nonatomic, strong) IBOutlet AnalogJoystickView *leftJoystickView;
@property (nonatomic, strong) IBOutlet AnalogJoystickView *rightJoystickView;
@property (nonatomic, strong) IBOutletCollection(UIButton) NSArray<UIButton *> *buttons;
@property (nonatomic, strong) UIView *contentView;
@property (nonatomic, assign) CGFloat hitSlop;
@property (atomic, assign) BOOL sdlWasInit;
// Whether the controls show at all (TouchControls decides). Hidden, they take
// no touches and their controller goes, so that a real one takes its player;
// the next touch after they show again brings it back.
@property (nonatomic, assign, readonly) BOOL controlsShown;
- (void)resetHideTimer;
- (void)setControlsShown:(BOOL)shown;
// The game's choice of buttons (command 0x8003, touchButtonsMessage in the
// engine's src/touch_glyphs.go): bit i of the low ten shows button i of a, b,
// c, x, y, z, d, w, Start and Menu; bits 10 to 13 are one more than the grid
// button whose place Start takes, 0 for none; bit 14 is the grid layout, and
// bits 16 to 19 the layout (TouchLayout), 0 from a game before them.
- (void)setButtonsShown:(int)message;
// The layout shown (TouchLayout).
@property (nonatomic, assign, readonly) int layout;
// Where on an arcade panel each attack button sits (command 0x8006,
// touchPanelMessage; TouchPanelDefaultMap), 0 for the default.
- (void)setPanelMap:(uint32_t)map;

// The player's changes to the current layout (TouchCustomLayout), applied
// over its own places; the layout editor (TouchLayoutEditor) sets them as
// the player drags, and saves or drops them.
- (TouchCustomLayout *)custom;
- (void)setCustom:(TouchCustomLayout *)custom;
- (void)saveCustom;
// While the player edits the layout, the buttons play nothing: everything a
// finger holds is let go, and the controls neither fade nor take presses.
@property (nonatomic, assign) BOOL editing;
// The element (an index in TouchEditNames) drawn at point, in this view's
// coordinates, or -1.
- (int)elementAtPoint:(CGPoint)point;
// Where element i is drawn, in this view's coordinates; CGRectNull while it
// is hidden.
- (CGRect)elementRect:(int)i;
// Element i's centre and size (diameter) in the layout, before the player's
// changes; NO while it has none.
- (BOOL)layoutPlace:(int)i centre:(CGPoint *)centre size:(CGFloat *)size;
// Shows on each button the glyph image the game wrote for it in dir
// (<name>.png), or its letter where it wrote none.
- (void)loadGlyphsFromDirectory:(NSString *)dir;

@end
