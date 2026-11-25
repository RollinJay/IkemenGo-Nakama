//
//  GamepadInputs.h
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import <Foundation/Foundation.h>

typedef NS_OPTIONS(uint16_t, GamepadButton) {
    GamepadButtonNone = 0,
    GamepadButtonA    = 1 << 1,
    GamepadButtonB    = 1 << 2,
    GamepadButtonC    = 1 << 3,
    GamepadButtonX    = 1 << 4,
    GamepadButtonY    = 1 << 5,
    GamepadButtonZ    = 1 << 6,
    GamepadButtonS    = 1 << 7,
    GamepadButtonM    = 1 << 8,
    GamepadButtonD    = 1 << 9,
    GamepadButtonW    = 1 << 10,
};
