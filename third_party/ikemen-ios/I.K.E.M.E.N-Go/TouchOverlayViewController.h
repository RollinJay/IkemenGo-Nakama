//
//  TouchOverlayViewController.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import <UIKit/UIKit.h>
#import "AnalogJoystickView.h"
#import "TouchOverlayView.h"
#import "ViewController.h"

@interface TouchOverlayViewController : UIViewController <GameFolderButtonDelegate>
@property (nonatomic, weak) ViewController *parentVC;
@property (nonatomic, strong) TouchOverlayView *overlayView;
@property (nonatomic, strong) IBOutlet UIButton *gameFolderButton;
@property (nonatomic, strong) IBOutlet UIView *vewlixButtonView;
@property (nonatomic, strong) IBOutlet DPadView *dPadView;
@property (nonatomic, strong) IBOutlet AnalogJoystickView *leftJoystickView;
@property (nonatomic, strong) IBOutlet AnalogJoystickView *rightJoystickView;
@property (nonatomic, strong) IBOutletCollection(UIButton) NSArray<UIButton *> *buttons;
@end
