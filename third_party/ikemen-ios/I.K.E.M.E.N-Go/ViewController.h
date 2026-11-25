//
//  ViewController.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/24/26.
//

#import <UIKit/UIKit.h>
#import "AnalogJoystickView.h"
#import "TouchOverlayView.h"
#import "SDLWindowFix.h"

@interface ViewController : UIViewController <UIDocumentPickerDelegate, CALayerDelegate, GameFolderButtonDelegate>
@property (nonatomic, strong) CADisplayLink *sdlFixDisplayLink;
@property (nonatomic, strong) IBOutlet id<GameFolderButtonDelegate> gameFolderButtonTapped;
@end

