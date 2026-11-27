//
//  VIrtualGamepadButton.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "VirtualGamepadButton.h"

@implementation VirtualGamepadButton

// Returning NO forces the parent TouchOverlayView to handle all hit testing
- (BOOL)pointInside:(CGPoint)point withEvent:(UIEvent *)event {
    return NO;
}

@end
