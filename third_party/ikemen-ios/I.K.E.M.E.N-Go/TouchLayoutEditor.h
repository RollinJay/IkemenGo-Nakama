//
//  TouchLayoutEditor.h
//  I.K.E.M.E.N-Go
//
//  The player's editor of the on-screen buttons, over them, as the Android
//  app's TouchLayoutEditor: a finger drags any button, the d-pad, Start or
//  Menu; two fingers pinch the one last touched bigger or smaller, as do the
//  Smaller and Bigger buttons (x1.15 a tap); Opacity fades them all (100, 80,
//  60, 40%). Save keeps the changes for the current layout (each layout keeps
//  its own, TouchCustomLayout), Reset goes back to the layout's own places
//  (Save to keep that), and Cancel leaves without saving. The buttons play
//  nothing while it is open (TouchOverlayView editing).
//

#import <UIKit/UIKit.h>
#import "TouchOverlayView.h"

@interface TouchLayoutEditor : UIView

// Opens over overlay (its subview, the size of it); onClose runs once it is
// closed, by Save or Cancel, and the editor has left the overlay.
- (instancetype)initWithOverlay:(TouchOverlayView *)overlay onClose:(void (^)(void))onClose;

- (void)reset;
- (void)cancel;
- (void)save;
// Closes without saving, and without giving the overlay back what it had:
// for when the layout changed under it.
- (void)discard;

@end
