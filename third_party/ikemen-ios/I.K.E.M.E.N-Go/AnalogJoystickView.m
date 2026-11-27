//
//  AnalogJoystickView.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/24/26.
//

#import "AnalogJoystickView.h"

#import <Foundation/Foundation.h>

@interface AnalogJoystickView()

@property (nonatomic, strong) UIView *baseView;
@property (nonatomic, strong) UIView *handleView;
@property (nonatomic, assign, readonly) CGFloat maxRadius;

@end

IB_DESIGNABLE
@implementation AnalogJoystickView

- (instancetype)initWithFrame:(CGRect)frame {
    self = [super initWithFrame:frame];
    if (self) {
        [self setupViews];
    }
    return self;
}

- (instancetype)initWithCoder:(NSCoder *)coder {
    self = [super initWithCoder:coder];
    if (self) {
        [self setupViews];
    }
    return self;
}

- (CGFloat)maxRadius {
    CGFloat minDimension = fmin(self.bounds.size.width, self.bounds.size.height);
    return minDimension / 2.0;
}

- (void)setupViews {
    if (_baseView) {
        return;
    }
    self.multipleTouchEnabled = NO;
    
    // Base outer circle
    _baseView = [[UIView alloc] init];
    _baseView.backgroundColor = [[UIColor whiteColor] colorWithAlphaComponent:0.2];
    //_baseView.layer.borderWidth = 2.0;
    [self addSubview:_baseView];
    
    // Center handle
    _handleView = [[UIView alloc] init];
    _handleView.backgroundColor = [[UIColor whiteColor] colorWithAlphaComponent:0.7];
    [self addSubview:_handleView];
}

- (void)layoutSubviews {
    [super layoutSubviews];
    
    [self setupViews];
    
    // Fallback size for IB canvas if bounds are 0
    CGFloat minDimension = fmin(self.bounds.size.width, self.bounds.size.height);
    if (minDimension <= 0) {
        minDimension = 120.0;
    }
    
    // Frame outer base circle
    self.baseView.frame = CGRectMake(0, 0, minDimension, minDimension);
    self.baseView.layer.cornerRadius = minDimension / 2.0;
    self.baseView.clipsToBounds = YES;
    
    // Frame center handle (50% size of base)
    CGFloat handleSize = minDimension * 0.5;
    self.handleView.bounds = CGRectMake(0, 0, handleSize, handleSize);
    self.handleView.layer.cornerRadius = handleSize / 2.0;
    self.handleView.center = CGPointMake(minDimension / 2.0, minDimension / 2.0);
    self.handleView.clipsToBounds = YES;
    
    [self bringSubviewToFront:self.handleView];
}

#pragma mark - Touch Event Handling

- (void)touchesBegan:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    UITouch *touch = [touches anyObject];
    if (touch) {
        [self updateHandleForTouchPoint:[touch locationInView:self]];
    }
}

- (void)touchesMoved:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    UITouch *touch = [touches anyObject];
    if (touch) {
        [self updateHandleForTouchPoint:[touch locationInView:self]];
    }
}

- (void)touchesEnded:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self resetHandle];
}

- (void)touchesCancelled:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self resetHandle];
}

#pragma mark - Stick Logic

- (void)updateHandleForTouchPoint:(CGPoint)touchPoint {
    CGPoint center = CGPointMake(CGRectGetMidX(self.bounds), CGRectGetMidY(self.bounds));
    CGFloat dx = touchPoint.x - center.x;
    CGFloat dy = touchPoint.y - center.y;
    CGFloat distance = hypotf(dx, dy);
    
    CGFloat clampRadius = self.maxRadius * 0.5; // Maximum travel distance inside base
    
    if (distance <= clampRadius) {
        self.handleView.center = touchPoint;
    } else {
        // Clamp position
        CGFloat angle = atan2f(dy, dx);
        CGFloat clampedX = center.x + cosf(angle) * clampRadius;
        CGFloat clampedY = center.y + sinf(angle) * clampRadius;
        self.handleView.center = CGPointMake(clampedX, clampedY);
    }
    
    // Calculate normalized [-1.0,1.0] coordinates
    CGFloat normX = (self.handleView.center.x - center.x) / clampRadius;
    CGFloat normY = (self.handleView.center.y - center.y) / clampRadius;
    
    if ([self.delegate respondsToSelector:@selector(joystick:didMoveX:y:)]) {
        // Invert Y to match standard joystick direction
        [self.delegate joystick:self didMoveX:normX y:-normY];
    }
}

- (void)resetHandle {
    CGPoint center = CGPointMake(CGRectGetMidX(self.bounds), CGRectGetMidY(self.bounds));
    
    [UIView animateWithDuration:0.1 animations:^{
        self.handleView.center = center;
    }];
    
    if ([self.delegate respondsToSelector:@selector(joystick:didMoveX:y:)]) {
        [self.delegate joystick:self didMoveX:0.0 y:0.0];
    }
}
@end
