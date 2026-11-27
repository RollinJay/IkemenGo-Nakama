package main

// Commands the game sends the Android and iOS apps (sendToApp), with an int
// parameter. On Android they reach SDLActivity.onUnhandledMessage (SDL leaves
// commands from 0x8000 to apps); on iOS, the handler the app gave
// IkemenSetAppMessageHandler (third_party/ikemen-ios's TouchControls).
const (
	appMsgTextEntry    = 0x8001 // a text box opened (1) or closed (0)
	appMsgTouchGlyphs  = 0x8002 // the touch buttons' glyph images changed
	appMsgTouchButtons = 0x8003 // the touch buttons to show, Start's place and the layout (touchButtonsMessage)
	appMsgControllers  = 0x8004 // the number of real controllers connected (iOS only: appWantsControllerCount)
	appMsgTouchEdit    = 0x8005 // open the on-screen buttons' editor (1)
	appMsgTouchPanel   = 0x8006 // the attack buttons' places on an arcade panel (touchPanelMessage)
)
