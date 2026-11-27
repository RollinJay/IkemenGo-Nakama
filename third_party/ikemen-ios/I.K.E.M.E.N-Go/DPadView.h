//
//  DPadView.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import <UIKit/UIKit.h>

typedef NS_OPTIONS(NSUInteger, DPadDirection) {
    DPadDirectionNeutral = 0,
    DPadDirectionUp      = 1 << 0,
    DPadDirectionDown    = 1 << 1,
    DPadDirectionLeft    = 1 << 2,
    DPadDirectionRight   = 1 << 3,
};

@class DPadView;

@protocol DPadDelegate <NSObject>
// Used to implement DPad emulation
- (void)dPad:(DPadView *)dPad didChangeDirection:(DPadDirection)direction;
@end

IB_DESIGNABLE
@interface DPadView : UIView

@property (nonatomic, weak) IBOutlet id<DPadDelegate> delegate;

@property (nonatomic, assign) IBInspectable CGFloat deadZone;

// Visual offset in pt. during roll (default 6.0pt.)
@property (nonatomic, assign) IBInspectable CGFloat maxDiscOffset;

@end
