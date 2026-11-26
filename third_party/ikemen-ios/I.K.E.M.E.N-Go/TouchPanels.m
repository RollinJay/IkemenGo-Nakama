//
//  TouchPanels.m
//  I.K.E.M.E.N-Go
//
//  See TouchPanels.h. Keep the tables and sums in step with the Android app's
//  TouchPanels.java.
//

#import "TouchPanels.h"

NSString * const TouchEditNames[TOUCH_EDIT_ELEMENTS] = {
    @"dpad", @"a", @"b", @"c", @"x", @"y", @"z", @"d", @"w", @"start", @"menu",
};

static NSString * const LayoutNames[] = {@"classic", @"grid", @"straight", @"vewlix", @"arch", @"staggered"};
static const int LayoutCount = 6;

const uint32_t TouchPanelDefaultMap = 4u | 5u << 4 | 6u << 8 | 8u << 12 | 1u << 16 | 2u << 20 | 3u << 24 | 7u << 28;

static const CGFloat Panels[4][TOUCH_PANEL_PLACES][2] = {
    // Straight: two straight rows of four, 36 mm apart, the first column
    // 95 mm right of the stick, the rows 18 mm above and below it (203 mm
    // overall).
    {{95, -18}, {131, -18}, {167, -18}, {203, -18},
     {95, 18}, {131, 18}, {167, 18}, {203, 18}},
    // Vewlix: the stick 95 mm left of the second column (59 + 36 mm), the
    // first column 31.25 mm left of that, then 35 and 36 mm; the top row 9,
    // 27, 36 and 36 mm above the stick, the bottom row 27 and 9 mm below it,
    // then level (197.25 mm overall, from the drawing's 95 mm reference left
    // of the stick).
    {{63.75, -9}, {95, -27}, {130, -36}, {166, -36},
     {63.75, 27}, {95, 9}, {130, 0}, {166, 0}},
    // Arch: the first column 95 mm right of the stick, then 31.25, 35 and
    // 35 mm; the second column highest (steps of 9, 9 and 18 mm), the top row
    // 18, 36, 27 and 18 mm above the stick and the bottom row 18, 0, 9 and
    // 18 mm below it (196.25 mm overall).
    {{95, -18}, {126.25, -36}, {161.25, -27}, {196.25, -18},
     {95, 18}, {126.25, 0}, {161.25, 9}, {196.25, 18}},
    // Staggered: the bottom row 95, 33, 36 and 36 mm along, level with the
    // stick but for its first button 14 mm below; the top row 7 mm further
    // right, 38.5 mm above the stick but for its first button 24.5 mm above
    // (steps of 14, 10, 14.5 and 14 mm; 207 mm overall).
    {{102, -24.5}, {135, -38.5}, {171, -38.5}, {207, -38.5},
     {95, 14}, {128, 0}, {164, 0}, {200, 0}},
};
// A panel's buttons are 30 mm across, as the drawings' are; the d-pad 64.
static const CGFloat ButtonMM = 30;
static const CGFloat DPadMM = 64;
// The panel takes at most this share of the height, from the bottom, and its
// buttons and d-pad are at least this big, in points (the Android app's dp).
static const CGFloat MaxHeight = 0.45;
static const CGFloat MinButtonPt = 48;
static const CGFloat MinDPadPt = 120;
static const CGFloat MarginPt = 16;

NSString *TouchLayoutName(int layout) {
    return layout >= 1 && layout <= LayoutCount ? LayoutNames[layout - 1] : LayoutNames[0];
}

BOOL TouchLayoutIsPanel(int layout) {
    return layout >= TouchLayoutStraight && layout <= TouchLayoutStaggered;
}

int TouchPanelPlaceOf(uint32_t map, int b) {
    for (int p = 0; p < TOUCH_PANEL_PLACES; p++) {
        if (((map >> (4 * p)) & 0xF) == (uint32_t)(b + 1)) {
            return p;
        }
    }
    return -1;
}

const CGFloat (*TouchPanelMillimetres(int layout))[2] {
    return TouchLayoutIsPanel(layout) ? Panels[layout - TouchLayoutStraight] : NULL;
}

TouchPanelPlacement TouchPanelPlace(int layout, CGRect area) {
    TouchPanelPlacement out;
    memset(&out, 0, sizeof out);
    if (!TouchLayoutIsPanel(layout)) {
        return out;
    }
    const CGFloat (*panel)[2] = Panels[layout - TouchLayoutStraight];
    CGFloat r = ButtonMM / 2, rd = DPadMM / 2;
    CGFloat minX = -rd, maxX = rd, minY = -rd, maxY = rd;
    // The rightmost and lowest button centres.
    CGFloat lastX = 0, lowY = -rd;
    for (int p = 0; p < TOUCH_PANEL_PLACES; p++) {
        minX = MIN(minX, panel[p][0] - r);
        maxX = MAX(maxX, panel[p][0] + r);
        minY = MIN(minY, panel[p][1] - r);
        maxY = MAX(maxY, panel[p][1] + r);
        lastX = MAX(lastX, panel[p][0]);
        lowY = MAX(lowY, panel[p][1]);
    }
    CGFloat w = area.size.width, h = area.size.height;
    CGFloat availW = w - 2 * MarginPt, availH = h * MaxHeight - MarginPt;
    CGFloat s = MAX(0, MIN(availW / (maxX - minX), availH / (maxY - minY)));
    out.button = MAX(ButtonMM * s, MinButtonPt);
    out.dpad = MAX(DPadMM * s, MinDPadPt);
    // The stick's row; the block's stick, from which its buttons are
    // measured; the d-pad's centre.
    CGFloat oy = area.origin.y + h - MarginPt - MAX(out.dpad / 2, lowY * s + out.button / 2);
    CGFloat bx = area.origin.x + w - MarginPt - out.button / 2 - lastX * s;
    CGFloat dx = area.origin.x + MarginPt + out.dpad / 2;
    for (int p = 0; p < TOUCH_PANEL_PLACES; p++) {
        out.centre[p] = CGPointMake(bx + panel[p][0] * s, oy + panel[p][1] * s);
    }
    out.dpadCentre = CGPointMake(dx, oy);
    return out;
}

static NSString *CustomKey(int layout) {
    return [@"IKEMENTouchCustom_" stringByAppendingString:TouchLayoutName(layout)];
}

static NSString *AlphaKey(int layout) {
    return [@"IKEMENTouchAlpha_" stringByAppendingString:TouchLayoutName(layout)];
}

// A number as the Android app writes it (Locale.ROOT); NO for anything else.
static BOOL ParseNumber(NSString *s, double *v) {
    NSScanner *scanner = [NSScanner scannerWithString:s];
    scanner.locale = nil; // '.' whatever the device's language
    scanner.charactersToBeSkipped = [NSCharacterSet whitespaceCharacterSet];
    return [scanner scanDouble:v] && scanner.isAtEnd;
}

@implementation TouchCustomLayout {
    BOOL _has[TOUCH_EDIT_ELEMENTS];
    CGFloat _pos[TOUCH_EDIT_ELEMENTS][3];
}

- (instancetype)init {
    self = [super init];
    if (self) {
        _alpha = 1.0;
    }
    return self;
}

- (id)copyWithZone:(NSZone *)zone {
    TouchCustomLayout *c = [[TouchCustomLayout allocWithZone:zone] init];
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        if (_has[i]) {
            [c setElement:i centre:CGPointMake(_pos[i][0], _pos[i][1]) size:_pos[i][2]];
        }
    }
    c.alpha = self.alpha;
    return c;
}

- (BOOL)hasPosition:(int)element {
    return element >= 0 && element < TOUCH_EDIT_ELEMENTS && _has[element];
}

- (CGPoint)centreOf:(int)element {
    return [self hasPosition:element] ? CGPointMake(_pos[element][0], _pos[element][1]) : CGPointZero;
}

- (CGFloat)sizeOf:(int)element {
    return [self hasPosition:element] ? _pos[element][2] : 1.0;
}

- (void)setElement:(int)element centre:(CGPoint)centre size:(CGFloat)size {
    if (element < 0 || element >= TOUCH_EDIT_ELEMENTS) {
        return;
    }
    _has[element] = YES;
    _pos[element][0] = TouchClamp(centre.x, 0, 1);
    _pos[element][1] = TouchClamp(centre.y, 0, 1);
    _pos[element][2] = TouchClamp(size, TOUCH_CUSTOM_MIN_SIZE, TOUCH_CUSTOM_MAX_SIZE);
}

- (BOOL)isEmpty {
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        if (_has[i]) {
            return NO;
        }
    }
    return self.alpha >= 1.0;
}

- (NSString *)encoded {
    NSMutableArray<NSString *> *entries = [NSMutableArray array];
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        if (!_has[i]) {
            continue;
        }
        // stringWithFormat: is not localized: '.' as the Android app's
        // Locale.ROOT.
        [entries addObject:[NSString stringWithFormat:@"%@:%.4f,%.4f,%.3f", TouchEditNames[i],
                            (double)_pos[i][0], (double)_pos[i][1], (double)_pos[i][2]]];
    }
    return [entries componentsJoinedByString:@";"];
}

+ (instancetype)decode:(NSString *)s alpha:(CGFloat)alpha {
    TouchCustomLayout *c = [[TouchCustomLayout alloc] init];
    c.alpha = TouchClamp(alpha, TOUCH_CUSTOM_MIN_ALPHA, 1.0);
    for (NSString *entry in [s ?: @"" componentsSeparatedByString:@";"]) {
        NSRange colon = [entry rangeOfString:@":"];
        if (colon.location == NSNotFound) {
            continue;
        }
        NSString *name = [[entry substringToIndex:colon.location]
                          stringByTrimmingCharactersInSet:[NSCharacterSet whitespaceCharacterSet]];
        NSArray<NSString *> *v = [[entry substringFromIndex:colon.location + 1] componentsSeparatedByString:@","];
        if (v.count != 3) {
            continue;
        }
        double x, y, size;
        if (!ParseNumber(v[0], &x) || !ParseNumber(v[1], &y) || !ParseNumber(v[2], &size)) {
            continue;
        }
        for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
            if ([TouchEditNames[i] isEqualToString:name]) {
                [c setElement:i centre:CGPointMake(x, y) size:size];
            }
        }
    }
    return c;
}

+ (instancetype)loadForLayout:(int)layout {
    NSUserDefaults *d = [NSUserDefaults standardUserDefaults];
    NSNumber *alpha = [d objectForKey:AlphaKey(layout)];
    return [self decode:[d stringForKey:CustomKey(layout)] alpha:alpha ? alpha.doubleValue : 1.0];
}

- (void)saveForLayout:(int)layout {
    NSUserDefaults *d = [NSUserDefaults standardUserDefaults];
    NSString *s = [self encoded];
    if (s.length == 0) {
        [d removeObjectForKey:CustomKey(layout)];
    } else {
        [d setObject:s forKey:CustomKey(layout)];
    }
    if (self.alpha >= 1.0) {
        [d removeObjectForKey:AlphaKey(layout)];
    } else {
        [d setDouble:self.alpha forKey:AlphaKey(layout)];
    }
}

@end
