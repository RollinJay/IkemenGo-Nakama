//
//  TouchPanels.h
//  I.K.E.M.E.N-Go
//
//  The on-screen layouts the game chooses from (touchLayouts in the engine's
//  src/touch_glyphs.go, numbered alike), the arcade panels' coordinates, and
//  the player's own changes to a layout, kept per layout: the Android app's
//  TouchPanels (third_party/ikemen-droid), with the same tables and sums.
//
//  The four panels are slagcoin.com's Japanese 8-button layouts, read from
//  its drawings: each button's centre in millimetres from the stick's, x to
//  the right and y down, the top row left to right and then the bottom row.
//  The d-pad goes where the stick is, and the whole panel is scaled, keeping
//  its proportions, to fit the screen.
//

#import <UIKit/UIKit.h>

typedef NS_ENUM(int, TouchLayout) {
    TouchLayoutClassic = 1,
    TouchLayoutGrid = 2,
    TouchLayoutStraight = 3,
    TouchLayoutVewlix = 4,
    TouchLayoutArch = 5,
    TouchLayoutStaggered = 6,
};

// A panel's eight places: top row left to right, then bottom row.
#define TOUCH_PANEL_PLACES 8

// What the player can move, resize and fade, in this order: the d-pad, then
// the buttons a, b, c, x, y, z, d, w, Start and Menu (TouchOverlayView's
// button indexes 0 to 9, one on). The names are the preferences' (and the
// Android app's).
#define TOUCH_EDIT_ELEMENTS 11
extern NSString * const TouchEditNames[TOUCH_EDIT_ELEMENTS];

// Where each button sits by default (the game's [Touch Controls] panel.top
// and panel.bottom, command 0x8006): place p holds, in the 4 bits from 4p,
// one more than the button's index (a, b, c, x, y, z, d, w), 0 for none. The
// default is x, y, z, w over a, b, c, d.
extern const uint32_t TouchPanelDefaultMap;

// The layout's name, as the game and the preferences spell it.
NSString *TouchLayoutName(int layout);
BOOL TouchLayoutIsPanel(int layout);
// The place (0 to 7) of grid button b (0 to 7) in map, or -1.
int TouchPanelPlaceOf(uint32_t map, int b);

// A panel laid out in an area: the centre and diameter of each place, and
// the d-pad's, in the area's points.
typedef struct {
    CGPoint centre[TOUCH_PANEL_PLACES];
    CGFloat button;
    CGPoint dpadCentre;
    CGFloat dpad;
} TouchPanelPlacement;

// Lays panel layout out in area, as the Android app does in its pixels with
// dp for points: the scale fits the whole panel as drawn in the width less
// margins and the bottom 45% of the height (MaxHeight), and sizes the d-pad
// and the buttons alike (buttons at least 48 points across, the d-pad 120);
// then the d-pad sits a margin from the left edge and the eight buttons, as
// one block keeping the drawing's shape, a margin from the right, the
// stick's row shared and the lower of them a margin from the bottom.
TouchPanelPlacement TouchPanelPlace(int layout, CGRect area);

// The panel's coordinates in mm (8 x {x, y}), or NULL for another layout.
const CGFloat (*TouchPanelMillimetres(int layout))[2];

// The player's changes to one layout: for each element (TouchEditNames), none
// or {x, y, size}: its centre as a share of the overlay's width and height,
// and its size as a multiple of the layout's own; and the opacity of them
// all. Kept in NSUserDefaults per layout, in the Android app's format:
// IKEMENTouchCustom_<layout> = "name:x,y,size;..." and
// IKEMENTouchAlpha_<layout> = the opacity, from 0.2 to 1 (absent for 1).
@interface TouchCustomLayout : NSObject <NSCopying>
@property (nonatomic, assign) CGFloat alpha;
- (BOOL)hasPosition:(int)element;
// {x, y, size}; only for an element that has one.
- (CGPoint)centreOf:(int)element;
- (CGFloat)sizeOf:(int)element;
- (void)setElement:(int)element centre:(CGPoint)centre size:(CGFloat)size;
- (BOOL)isEmpty;
+ (instancetype)loadForLayout:(int)layout;
- (void)saveForLayout:(int)layout;
// The preferences' string, and back (also for tests).
- (NSString *)encoded;
+ (instancetype)decode:(NSString *)s alpha:(CGFloat)alpha;
@end

#define TOUCH_CUSTOM_MIN_ALPHA 0.2
#define TOUCH_CUSTOM_MIN_SIZE 0.5
#define TOUCH_CUSTOM_MAX_SIZE 2.5

static inline CGFloat TouchClamp(CGFloat v, CGFloat lo, CGFloat hi) {
    return MAX(lo, MIN(hi, v));
}
