//
//  AnalogJoystickView.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/24/26.
//

#import <UIKit/UIKit.h>

@class AnalogJoystickView;

@protocol AnalogJoystickDelegate <NSObject>
// Returns normalized x and y values between [-1.0,1.0]
- (void)joystick:(AnalogJoystickView *)joystick didMoveX:(CGFloat)x y:(CGFloat)y;
@end

IB_DESIGNABLE
@interface AnalogJoystickView : UIView
@property (nonatomic, weak) id<AnalogJoystickDelegate> delegate;
@end
