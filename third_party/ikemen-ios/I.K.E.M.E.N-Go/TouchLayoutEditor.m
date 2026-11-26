//
//  TouchLayoutEditor.m
//  I.K.E.M.E.N-Go
//
//  See TouchLayoutEditor.h. Like TouchControls, it uses target-action and
//  touches only, nothing that goes through the main dispatch queue, which
//  the game holds while it runs.
//

#import "TouchLayoutEditor.h"

static const CGFloat Alphas[] = {1.0, 0.8, 0.6, 0.4};
static const int AlphaCount = 4;
static const CGFloat ResizeStep = 1.15;

static NSString *Str(NSString *key, NSString *english) {
    return NSLocalizedStringWithDefaultValue(key, nil, [NSBundle mainBundle], english, @"Touch layout editor");
}

@interface TouchLayoutEditor ()
@property (nonatomic, weak) TouchOverlayView *overlay;
@property (nonatomic, copy) void (^onClose)(void);
@property (nonatomic, strong) TouchCustomLayout *saved;
@property (nonatomic, strong) TouchCustomLayout *working;
@property (nonatomic, strong) UILabel *titleLabel;
@property (nonatomic, strong) UIButton *opacityButton;
@property (nonatomic, strong) UIView *bar;
// The element being dragged (an index in TouchEditNames), the touch that
// drags it and its offset from the element's centre, and the one last
// touched, which the resize buttons and a pinch resize.
@property (nonatomic, assign) int dragging;
@property (nonatomic, weak) UITouch *dragTouch;
@property (nonatomic, assign) CGPoint offset;
@property (nonatomic, assign) int selected;
@property (nonatomic, assign) BOOL closed;
@end

@implementation TouchLayoutEditor

- (instancetype)initWithOverlay:(TouchOverlayView *)overlay onClose:(void (^)(void))onClose {
    self = [super initWithFrame:overlay.bounds];
    if (!self) {
        return nil;
    }
    _overlay = overlay;
    _onClose = [onClose copy];
    _saved = [overlay.custom copy];
    _working = [_saved copy];
    _dragging = -1;
    _selected = -1;
    self.autoresizingMask = UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
    self.opaque = NO;
    self.backgroundColor = [UIColor colorWithWhite:0 alpha:0.2];
    self.multipleTouchEnabled = YES;
    self.contentMode = UIViewContentModeRedraw;
    [self buildBar];

    UIPinchGestureRecognizer *pinch = [[UIPinchGestureRecognizer alloc] initWithTarget:self action:@selector(pinched:)];
    [self addGestureRecognizer:pinch];

    [overlay addSubview:self];
    [overlay setCustom:self.working];
    [self updateTitle];
    return self;
}

- (UIButton *)action:(NSString *)title selector:(SEL)selector {
    UIButton *b = [UIButton buttonWithType:UIButtonTypeSystem];
    [b setTitle:title forState:UIControlStateNormal];
    [b setTitleColor:[UIColor whiteColor] forState:UIControlStateNormal];
    b.titleLabel.font = [UIFont systemFontOfSize:15];
    b.backgroundColor = [UIColor colorWithWhite:0.3 alpha:0.9];
    b.layer.cornerRadius = 8;
    b.contentEdgeInsets = UIEdgeInsetsMake(0, 10, 0, 10);
    [b addTarget:self action:selector forControlEvents:UIControlEventTouchUpInside];
    [b.heightAnchor constraintEqualToConstant:40].active = YES;
    [b.widthAnchor constraintGreaterThanOrEqualToConstant:56].active = YES;
    return b;
}

// The title and the action buttons, at the top in the middle.
- (void)buildBar {
    UIStackView *row = [[UIStackView alloc] initWithArrangedSubviews:@[
        [self action:Str(@"touchEditSmallerStr", @"Smaller") selector:@selector(smallerTapped:)],
        [self action:Str(@"touchEditBiggerStr", @"Bigger") selector:@selector(biggerTapped:)],
    ]];
    row.axis = UILayoutConstraintAxisHorizontal;
    row.spacing = 6;
    self.opacityButton = [self action:Str(@"touchEditOpacityStr", @"Opacity") selector:@selector(opacityTapped:)];
    [row addArrangedSubview:self.opacityButton];
    [row addArrangedSubview:[self action:Str(@"touchEditResetStr", @"Reset") selector:@selector(resetTapped:)]];
    [row addArrangedSubview:[self action:Str(@"touchEditCancelStr", @"Cancel") selector:@selector(cancelTapped:)]];
    [row addArrangedSubview:[self action:Str(@"touchEditSaveStr", @"Save") selector:@selector(saveTapped:)]];

    self.titleLabel = [[UILabel alloc] init];
    self.titleLabel.textColor = [UIColor whiteColor];
    self.titleLabel.font = [UIFont systemFontOfSize:13];
    self.titleLabel.textAlignment = NSTextAlignmentCenter;
    self.titleLabel.numberOfLines = 2;

    UIStackView *bar = [[UIStackView alloc] initWithArrangedSubviews:@[self.titleLabel, row]];
    bar.axis = UILayoutConstraintAxisVertical;
    bar.alignment = UIStackViewAlignmentCenter;
    bar.spacing = 6;
    bar.layoutMarginsRelativeArrangement = YES;
    bar.layoutMargins = UIEdgeInsetsMake(6, 6, 6, 6);
    bar.backgroundColor = [UIColor colorWithWhite:0.125 alpha:0.8];
    bar.layer.cornerRadius = 12;
    bar.translatesAutoresizingMaskIntoConstraints = NO;
    [self addSubview:bar];
    [NSLayoutConstraint activateConstraints:@[
        [bar.topAnchor constraintEqualToAnchor:self.safeAreaLayoutGuide.topAnchor constant:4],
        [bar.centerXAnchor constraintEqualToAnchor:self.centerXAnchor],
        [bar.widthAnchor constraintLessThanOrEqualToAnchor:self.safeAreaLayoutGuide.widthAnchor constant:-8],
        [self.titleLabel.widthAnchor constraintLessThanOrEqualToAnchor:self.safeAreaLayoutGuide.widthAnchor constant:-20],
    ]];
    self.bar = bar;
}

- (void)updateTitle {
    NSString *what = self.selected >= 0 ? TouchEditNames[self.selected] : @"-";
    NSString *format = Str(@"touchEditTitleStr", @"Layout: %@. Drag a button or the d-pad; pinch or Smaller/Bigger to resize. Selected: %@");
    self.titleLabel.text = [NSString stringWithFormat:format, TouchLayoutName(self.overlay.layout), what];
    [self.opacityButton setTitle:[NSString stringWithFormat:@"%@ %ld%%", Str(@"touchEditOpacityStr", @"Opacity"),
                                  lround(self.working.alpha * 100)]
                        forState:UIControlStateNormal];
}

- (void)layoutSubviews {
    [super layoutSubviews];
    [self setNeedsDisplay];
}

#pragma mark - Touches

- (void)touchesBegan:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self.overlay resetHideTimer];
    if (self.dragTouch || [event touchesForView:self].count > 1) {
        // A second finger pinches: no more dragging.
        self.dragging = -1;
        self.dragTouch = nil;
        return;
    }
    UITouch *touch = touches.anyObject;
    CGPoint p = [touch locationInView:self.overlay];
    int i = [self.overlay elementAtPoint:p];
    if (i < 0) {
        return;
    }
    CGRect r = [self.overlay elementRect:i];
    self.dragging = i;
    self.dragTouch = touch;
    self.offset = CGPointMake(p.x - CGRectGetMidX(r), p.y - CGRectGetMidY(r));
    self.selected = i;
    [self updateTitle];
    [self setNeedsDisplay];
}

- (void)touchesMoved:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    if (self.dragging < 0 || !self.dragTouch || ![touches containsObject:self.dragTouch]) {
        return;
    }
    CGPoint p = [self.dragTouch locationInView:self.overlay];
    [self move:self.dragging to:CGPointMake(p.x - self.offset.x, p.y - self.offset.y)];
}

- (void)touchesEnded:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    if (self.dragTouch && [touches containsObject:self.dragTouch]) {
        self.dragging = -1;
        self.dragTouch = nil;
    }
}

- (void)touchesCancelled:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    // A pinch took them, or the system did.
    self.dragging = -1;
    self.dragTouch = nil;
}

- (void)pinched:(UIPinchGestureRecognizer *)pinch {
    if (pinch.state == UIGestureRecognizerStateBegan || pinch.state == UIGestureRecognizerStateChanged) {
        self.dragging = -1;
        self.dragTouch = nil;
        [self resize:pinch.scale];
        pinch.scale = 1.0;
    }
}

#pragma mark - Changes

// Element i's own place, starting from where the layout puts it. NO if it
// has none.
- (BOOL)ensureOwn:(int)i {
    if ([self.working hasPosition:i]) {
        return YES;
    }
    CGSize size = self.overlay.bounds.size;
    CGPoint c;
    if (size.width <= 0 || size.height <= 0 || ![self.overlay layoutPlace:i centre:&c size:NULL]) {
        return NO;
    }
    [self.working setElement:i centre:CGPointMake(c.x / size.width, c.y / size.height) size:1.0];
    return YES;
}

// Puts element i's centre at p, in the overlay's coordinates, kept on the
// screen.
- (void)move:(int)i to:(CGPoint)p {
    CGSize size = self.overlay.bounds.size;
    if (size.width <= 0 || size.height <= 0 || ![self ensureOwn:i]) {
        return;
    }
    [self.working setElement:i centre:CGPointMake(p.x / size.width, p.y / size.height) size:[self.working sizeOf:i]];
    [self.overlay setCustom:self.working];
    [self setNeedsDisplay];
}

- (void)resize:(CGFloat)factor {
    int i = self.selected;
    if (i < 0 || ![self ensureOwn:i]) {
        return;
    }
    [self.working setElement:i centre:[self.working centreOf:i] size:[self.working sizeOf:i] * factor];
    [self.overlay setCustom:self.working];
    [self setNeedsDisplay];
}

- (void)smallerTapped:(id)sender {
    [self resize:1.0 / ResizeStep];
}

- (void)biggerTapped:(id)sender {
    [self resize:ResizeStep];
}

- (void)opacityTapped:(id)sender {
    int next = 0;
    for (int i = 0; i < AlphaCount; i++) {
        if (fabs(Alphas[i] - self.working.alpha) < 0.01) {
            next = (i + 1) % AlphaCount;
            break;
        }
    }
    self.working.alpha = Alphas[next];
    [self.overlay setCustom:self.working];
    [self updateTitle];
    [self setNeedsDisplay];
}

- (void)resetTapped:(id)sender {
    [self reset];
}

- (void)cancelTapped:(id)sender {
    [self cancel];
}

- (void)saveTapped:(id)sender {
    [self save];
}

- (void)reset {
    self.working = [[TouchCustomLayout alloc] init];
    self.selected = -1;
    [self.overlay setCustom:self.working];
    [self updateTitle];
    [self setNeedsDisplay];
}

- (void)cancel {
    [self.overlay setCustom:self.saved];
    [self close];
}

- (void)save {
    [self.overlay setCustom:self.working];
    [self.overlay saveCustom];
    [self close];
}

- (void)discard {
    [self close];
}

- (void)close {
    if (self.closed) {
        return;
    }
    self.closed = YES;
    [self removeFromSuperview];
    void (^onClose)(void) = self.onClose;
    self.onClose = nil;
    if (onClose) {
        onClose();
    }
}

#pragma mark - Drawing

// Each element's outline, the one last touched highlighted.
- (void)drawRect:(CGRect)rect {
    UIColor *outline = [UIColor colorWithWhite:1 alpha:0.8];
    UIColor *highlight = [UIColor colorWithRed:1.0 green:0.835 blue:0.31 alpha:1.0];
    CGFloat dash[] = {8, 6};
    for (int i = 0; i < TOUCH_EDIT_ELEMENTS; i++) {
        CGRect r = [self.overlay elementRect:i];
        if (CGRectIsNull(r)) {
            continue;
        }
        r = [self convertRect:r fromView:self.overlay];
        CGFloat radius = MIN(r.size.width, r.size.height) / 2.0;
        UIBezierPath *path = [UIBezierPath bezierPathWithRoundedRect:r cornerRadius:radius];
        if (i == self.selected) {
            path.lineWidth = 3;
            [highlight setStroke];
        } else {
            path.lineWidth = 2;
            [path setLineDash:dash count:2 phase:0];
            [outline setStroke];
        }
        [path stroke];
    }
}

@end
