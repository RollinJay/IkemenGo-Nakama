//
//  main.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/24/26.
//

#import <UIKit/UIKit.h>
#import "AppDelegate.h"

int main(int argc, char * argv[]) {
#if TARGET_OS_SIMULATOR
    // The iOS Simulator's Metal driver (MTLSimDriver) aborts when MoltenVK
    // writes textures into Metal argument buffers (vkUpdateDescriptorSets ->
    // -[MTLSimArgumentEncoder setTexture:atIndex:]). Have MoltenVK bind
    // resources directly instead. Simulator builds only; devices keep
    // argument buffers. A value set from outside still wins.
    setenv("MVK_CONFIG_USE_METAL_ARGUMENT_BUFFERS", "0", 0);
#endif
    NSString * appDelegateClassName;
    @autoreleasepool {
        // Setup code that might create autoreleased objects goes here.
        appDelegateClassName = NSStringFromClass([AppDelegate class]);
    }
    return UIApplicationMain(argc, argv, nil, appDelegateClassName);
}
