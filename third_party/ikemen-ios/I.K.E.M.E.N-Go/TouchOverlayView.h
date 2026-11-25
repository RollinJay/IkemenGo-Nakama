//
//  TouchOverlayView.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "AnalogJoystickView.h"
#import "DPadView.h"
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
- (void)resetHideTimer;

@end
