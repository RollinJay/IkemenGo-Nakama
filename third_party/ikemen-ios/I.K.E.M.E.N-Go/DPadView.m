//
//  DPadView.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "DPadView.h"

@interface DPadView()

@property (nonatomic, assign) DPadDirection currentDirection;
@property (nonatomic, assign) CGPoint discOffset;

@end

IB_DESIGNABLE
@implementation DPadView

- (instancetype)initWithCoder:(NSCoder *)coder {
    self = [super initWithCoder:coder];
    if (self) {
        [self commonInit];
    }
    return self;
}

- (instancetype)initWithFrame:(CGRect)frame {
    self = [super initWithFrame:frame];
    if (self) {
        [self commonInit];
    }
    return self;
}

- (BOOL)pointInside:(CGPoint)point withEvent:(UIEvent *)event {
    return [super pointInside:point withEvent:event];
}


- (void)setDeadZone:(CGFloat)deadZone {
    _deadZone = deadZone;
    [self setNeedsDisplay];
}

- (void)setMaxDiscOffset:(CGFloat)maxDiscOffset {
    _maxDiscOffset = maxDiscOffset;
    [self setNeedsDisplay];
}

- (void)commonInit {
    self.multipleTouchEnabled = NO;
    self.opaque = NO;
    self.backgroundColor = [UIColor clearColor];
    // Set preview defaults if not already set
    if (_deadZone <= 0) {
        _deadZone = 0.20;
    }
    if (_maxDiscOffset <= 0) {
        _maxDiscOffset = 6.0;
    }
    _discOffset = CGPointZero;
    _currentDirection = DPadDirectionNeutral;
}

- (void)drawRect:(CGRect)rect {
    [super drawRect:rect];
    
    CGContextRef ctx = UIGraphicsGetCurrentContext();
    if (!ctx) {
        return;
    }
    
    // Fallback bounds
    CGFloat width = self.bounds.size.width > 0 ? self.bounds.size.width : 120.0;
    CGFloat height = self.bounds.size.height > 0 ? self.bounds.size.height : 120.0;
    
    CGPoint center = CGPointMake(width / 2.0, height / 2.0);
    CGFloat radius = MIN(self.bounds.size.width, self.bounds.size.height) / 2.0 - 4.0;
    
    // Draw the wall of the D-Pad
    CGRect padRect = CGRectMake(center.x - radius, center.y - radius, radius*2.0, radius*2.0);
    CGContextSetRGBFillColor(ctx, 0.08, 0.08, 0.10, 0.6);
    CGContextSetRGBStrokeColor(ctx, 0.25, 0.25, 0.28, 0.8);
    CGContextSetLineWidth(ctx, 2.0);
    CGContextFillEllipseInRect(ctx, padRect);
    CGContextStrokeEllipseInRect(ctx, padRect);
    
    // Draw 8-way direction markers
    CGContextSetRGBStrokeColor(ctx, 1.0, 1.0, 1.0, 0.25);
    CGContextSetLineWidth(ctx, kCGLineCapRound);
    CGFloat markerRadius = radius - 8.0;
    for (int i = 0; i < 8; i++) {
        CGFloat angle = i * M_PI_4;
        CGPoint innerP = CGPointMake(center.x + cosf(angle)*(markerRadius-6.0),
                                     center.y + sinf(angle)*(markerRadius-6.0));
        CGPoint outerP = CGPointMake(center.x + cosf(angle)*markerRadius,
                                     center.y + sinf(angle)*markerRadius);
        CGContextMoveToPoint(ctx, innerP.x, innerP.y);
        CGContextAddLineToPoint(ctx, outerP.x, outerP.y);
        CGContextStrokePath(ctx);
    }
    
    // Now draw the D-Pad BG
    CGPoint discCenter = CGPointMake(center.x + self.discOffset.x, center.y + self.discOffset.y);
    CGFloat discRadius = radius * 0.82;
    CGRect discRect = CGRectMake(discCenter.x - discRadius, discCenter.y - discRadius, discRadius*2.0, discRadius*2.0);
    
    // Base Disc gradient fill
    CGContextSaveGState(ctx);
    CGContextAddEllipseInRect(ctx, discRect);
    CGContextClip(ctx);
    CGFloat colors[] = {
        0.30, 0.32, 0.35, 0.95,
        0.18, 0.19, 0.21, 0.95
    };
    CGColorSpaceRef rgb = CGColorSpaceCreateDeviceRGB();
    CGGradientRef grad = CGGradientCreateWithColorComponents(rgb, colors, NULL, 2);
    CGContextDrawLinearGradient(ctx, grad,
                                CGPointMake(discCenter.x, discCenter.y - discRadius),
                                CGPointMake(discCenter.x, discCenter.y + discRadius), 0);
    CGGradientRelease(grad);
    CGColorSpaceRelease(rgb);
    CGContextRestoreGState(ctx);
    
    // Outer bevel rim of floating disc
    CGContextSetRGBStrokeColor(ctx, 0.50, 0.52, 0.55, 0.9);
    CGContextSetLineWidth(ctx, 1.5);
    CGContextStrokeEllipseInRect(ctx, discRect);
    
    // Thumb nub (deadZone)
    CGFloat nubRadius = discRadius * _deadZone;
    CGRect nubRect = CGRectMake(discCenter.x - nubRadius, discCenter.y - nubRadius, nubRadius*2.0, nubRadius*2.0);
    // Inner Concave Dip
    CGContextSetRGBFillColor(ctx, 0.12, 0.13, 0.15, 0.9);
    CGContextSetRGBStrokeColor(ctx, 0.40, 0.42, 0.45, 0.6);
    CGContextSetLineWidth(ctx, 1.0);
    CGContextFillEllipseInRect(ctx, nubRect);
    CGContextStrokeEllipseInRect(ctx, nubRect);
}

#pragma mark - Touch Behavior
- (void)touchesBegan:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self processTouch:[touches anyObject]];
}

- (void)touchesMoved:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self processTouch:[touches anyObject]];
}

- (void)touchesEnded:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self resetDPad];
}

- (void)touchesCancelled:(NSSet<UITouch *> *)touches withEvent:(UIEvent *)event {
    [self resetDPad];
}

- (void)processTouch:(UITouch *)touch {
    if (!touch) {
        return;
    }
    
    CGPoint point = [touch locationInView:self];
    CGPoint center = CGPointMake(CGRectGetMidX(self.bounds), CGRectGetMidY(self.bounds));
    CGFloat maxRadius = MIN(self.bounds.size.width, self.bounds.size.height) / 2.0;
    
    CGFloat dx = (point.x - center.x) / maxRadius;
    CGFloat dy = (point.y - center.y) / maxRadius;
    CGFloat dist = hypotf(dx, dy);
    
    // Deadzone Evaluation
    if (dist < _deadZone) {
        [self resetDPad];
        return;
    }
    
    // Angle Calculation
    CGFloat rad = atan2f(dy, dx);
    CGFloat deg = rad * (180.0 / M_PI);
    if (deg < 0) {
        deg += 360.0;
    }
    
    // Sector Mapping (wider diagonals version)
    DPadDirection newDir = DPadDirectionNeutral;
    if (deg >= 345 || deg < 15)       newDir = DPadDirectionRight;                     // Right (Narrower)
    else if (deg >= 15 && deg < 75)   newDir = DPadDirectionDown | DPadDirectionRight; // Down-Right (Wider)
    else if (deg >= 75 && deg < 105)  newDir = DPadDirectionDown;                      // Down (Narrower)
    else if (deg >= 105 && deg < 165) newDir = DPadDirectionDown | DPadDirectionLeft;  // Down-Left (Wider)
    else if (deg >= 165 && deg < 195) newDir = DPadDirectionLeft;                      // Left (Narrower)
    else if (deg >= 195 && deg < 255) newDir = DPadDirectionUp | DPadDirectionLeft;    // Up-Left (Wider)
    else if (deg >= 255 && deg < 285) newDir = DPadDirectionUp;                        // Up (Narrower)
    else if (deg >= 285 && deg < 345) newDir = DPadDirectionUp | DPadDirectionRight;   // Up-Right (Wider)
    
    // Sector Mapping (old, equal sectors version)
//    if (deg >= 337.5 || deg < 22.5)       newDir = DPadDirectionRight;
//    else if (deg >= 22.5 && deg < 67.5)   newDir = DPadDirectionDown | DPadDirectionRight;
//    else if (deg >= 67.5 && deg < 112.5)  newDir = DPadDirectionDown;
//    else if (deg >= 112.5 && deg < 157.5) newDir = DPadDirectionDown | DPadDirectionLeft;
//    else if (deg >= 157.5 && deg < 202.5) newDir = DPadDirectionLeft;
//    else if (deg >= 202.5 && deg < 247.5) newDir = DPadDirectionUp | DPadDirectionLeft;
//    else if (deg >= 247.5 && deg < 292.5) newDir = DPadDirectionUp;
//    else if (deg >= 292.5 && deg < 337.5) newDir = DPadDirectionUp | DPadDirectionRight;
    
    // Dynamic Vector Displacement Offset
    CGFloat normDist = MIN(dist, 1.0);
    CGPoint offVector = CGPointMake(cosf(rad)*normDist*_maxDiscOffset,
                                    sinf(rad)*normDist*_maxDiscOffset);
    
    [self updateDirection:newDir offset:offVector];
}

- (void)updateDirection:(DPadDirection)direction offset:(CGPoint)offset {
    self.discOffset = offset;
    [self setNeedsDisplay]; // Force vector redraw
    
    if (self.currentDirection != direction) {
        self.currentDirection = direction;
        
        if ([self.delegate respondsToSelector:@selector(dPad:didChangeDirection:)]) {
            [self.delegate dPad:self didChangeDirection:self.currentDirection];
        }
    }
}

- (void)resetDPad {
    [self updateDirection:DPadDirectionNeutral offset:CGPointZero];
}

@end
