package org.libsdl.app;

import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.ColorFilter;
import android.graphics.Paint;
import android.graphics.PixelFormat;
import android.graphics.Rect;
import android.graphics.RectF;
import android.graphics.drawable.Drawable;
import android.graphics.drawable.LayerDrawable;
import android.view.InputDevice;
import android.view.LayoutInflater;
import android.view.MotionEvent;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.RelativeLayout;

import org.ikemen_engine.ikemen_go.R;

import java.io.File;
import java.util.HashMap;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;

public class ControllerOverlay extends RelativeLayout {
    // The controller the buttons play as. The game recognises it by its name
    // (touchControlsName in src/system_sdl.go); without a vendor and product
    // ID, SDL keeps that name and maps its buttons from their mask. Its device
    // ID is one no real device has.
    static final String DEVICE_NAME = "IKEMEN Touch Controls";
    private static final int virtualDeviceId = 0x49444D00;

    // The buttons whose images the game writes as <name>.png, with the text
    // and background they have without one.
    private static final String[] GLYPH_NAMES = {"a", "b", "c", "x", "y", "z", "d", "w", "start", "menu", "up", "down", "left", "right"};
    private static final int[] GLYPH_VIEWS = {R.id.btn_a, R.id.btn_b, R.id.btn_c, R.id.btn_x, R.id.btn_y, R.id.btn_z, R.id.btn_d, R.id.btn_w,
            R.id.btn_start, R.id.btn_back, R.id.dp_up, R.id.dp_down, R.id.dp_left, R.id.dp_right};
    private final Map<Integer, CharSequence> plainText = new HashMap<>();
    private final Map<Integer, Drawable> plainBackground = new HashMap<>();
    private int hatX, hatY = 0;
    private final Map<Integer, Set<Integer>> pointerStates = new HashMap<>(); // PointerID -> Keycode/Axis
    private boolean isInitialized = false;

    public static int getPhysicalJoystickCount() {
        int count = 0;
        int[] deviceIds = InputDevice.getDeviceIds();
        for (int id : deviceIds) {
            InputDevice dev = InputDevice.getDevice(id);
            // Check if the device is a physical joystick or gamepad
            // OLD LOGIC: Checked only flags (Let sensors through)
            // NEW LOGIC: Checks isRealGamepad (Stricter filtering by querying for buttons)
            if (isRealGamepad(dev)) {
                count++;
            }
        }
        return count;
    }

    // Helper to distinguish real controllers from "Ghost" sensors
    static boolean isRealGamepad(InputDevice device) {
        if (device == null) return false;

        int sources = device.getSources();
        boolean isJoystick = ((sources & InputDevice.SOURCE_JOYSTICK) == InputDevice.SOURCE_JOYSTICK) ||
                ((sources & InputDevice.SOURCE_GAMEPAD) == InputDevice.SOURCE_GAMEPAD);

        // Must identify as joystick/gamepad
        if (!isJoystick) return false;

        // Ignore virtual devices (optional, prevents software loopbacks)
        if (device.isVirtual()) return false;

        // Ghost filter, does it have actual buttons?
        // Sensors report as Joysticks but have NO buttons.
        // Real gamepads always have at least one of these.
        int[] keysToCheck = {
                android.view.KeyEvent.KEYCODE_BUTTON_A,
                android.view.KeyEvent.KEYCODE_BUTTON_B,
                android.view.KeyEvent.KEYCODE_BUTTON_X,
                android.view.KeyEvent.KEYCODE_BUTTON_Y,
                android.view.KeyEvent.KEYCODE_BUTTON_START,
                android.view.KeyEvent.KEYCODE_BUTTON_SELECT,
        };

        boolean[] hasKeys = device.hasKeys(keysToCheck);
        for (boolean exists : hasKeys) {
            if (exists) return true; // It has any of these buttons, it's a real gamepad
        }

        return false; // No buttons, its a sensor, ignore it
    }

    @Override
    public boolean onInterceptTouchEvent(MotionEvent ev) {
        if (!isInitialized) {
            initializeVirtualController();
            isInitialized = true;
        }
        return true;
    }

    private int leftJoyPointerId = -1;
    private int rightJoyPointerId = -1;

    @Override
    public boolean onTouchEvent(MotionEvent event) {
        int action = event.getActionMasked();
        int index = event.getActionIndex();
        int pId = event.getPointerId(index);

        // Get raw coordinates for global hit detection
        float x = event.getX(index);
        float y = event.getY(index);

        if (action == MotionEvent.ACTION_DOWN || action == MotionEvent.ACTION_POINTER_DOWN) {
            // Check Joysticks first
            if (isViewAtLocation(findViewById(R.id.left_analog), x, y)) {
                leftJoyPointerId = pId;
            } else if (isViewAtLocation(findViewById(R.id.right_analog), x, y)) {
                rightJoyPointerId = pId;
            } else {
                // If not a stick, it's a button/dpad
                updatePointer(pId, getButtonsAt(x, y));
            }
        } else if (action == MotionEvent.ACTION_MOVE) {
            for (int i = 0; i < event.getPointerCount(); i++) {
                int movePId = event.getPointerId(i); // The stable ID

                // Get coordinates specifically for THIS pointer index
                float mx = event.getX(i);
                float my = event.getY(i);

                if (movePId == leftJoyPointerId) {
                    updateJoystickLogic(findViewById(R.id.left_analog), mx, my);
                } else if (movePId == rightJoyPointerId) {
                    updateJoystickLogic(findViewById(R.id.right_analog), mx, my);
                } else {
                    Set<Integer> currentButtons = getButtonsAt(mx, my);
                    Set<Integer> lastButtons = pointerStates.get(movePId);

                    // If the sets are different, update the physical state
                    if (!currentButtons.equals(lastButtons)) {
                        updatePointer(movePId, currentButtons);
                    }
                }
            }
        } else if (action == MotionEvent.ACTION_UP || action == MotionEvent.ACTION_POINTER_UP || action == MotionEvent.ACTION_CANCEL) {
            if (pId == leftJoyPointerId) {
                resetJoystick(findViewById(R.id.left_analog));
                leftJoyPointerId = -1;
            } else if (pId == rightJoyPointerId) {
                resetJoystick(findViewById(R.id.right_analog));
                rightJoyPointerId = -1;
            } else {
                releasePointer(pId);
            }
        }

        updateDpadState(event);
        return true;
    }

    private void updateJoystickLogic(View v, float x, float y) {
        if (!(v instanceof JoystickOverlay)) return;
        JoystickOverlay joy = (JoystickOverlay) v;

        // LOCAL CENTER CALCULATION
        float centerX = v.getLeft() + (v.getWidth() / 2f);
        float centerY = v.getTop() + (v.getHeight() / 2f);
        float radius = v.getWidth() / 3f;

        float dx = x - centerX;
        float dy = y - centerY;
        float distance = (float) Math.sqrt(dx * dx + dy * dy);

        if (distance > radius) {
            dx = (dx / distance) * radius;
            dy = (dy / distance) * radius;
        }

        joy.updateVisualPos((v.getWidth()/2f) + dx, (v.getHeight()/2f) + dy);
        joy.sendToSDL(dx / radius, dy / radius);
    }

    private void resetJoystick(View v) {
        if (!(v instanceof JoystickOverlay)) return;
        JoystickOverlay joy = (JoystickOverlay) v;
        joy.updateVisualPos(v.getWidth()/2f, v.getHeight()/2f);
        joy.sendToSDL(0, 0);
    }

    private void updatePointer(int pointerId, Set<Integer> newCodes) {
        Set<Integer> oldCodes = pointerStates.get(pointerId);
        if (oldCodes == null) oldCodes = new HashSet<>();
        // Find buttons that were RELEASED (in old set, not in new)
        for (Integer oldCode : oldCodes) {
            if (!newCodes.contains(oldCode)) {
                handleInput(oldCode, false);
            }
        }
        // Find buttons that were PRESSED (in new set, not in old)
        for (Integer newCode : newCodes) {
            if (!oldCodes.contains(newCode)) {
                handleInput(newCode, true);
            }
        }
        // Save the new state for this finger
        pointerStates.put(pointerId, newCodes);
    }

    private void releasePointer(int pointerId) {
        Set<Integer> lastCodes = pointerStates.remove(pointerId);
        if (lastCodes != null) {
            for (Integer code : lastCodes) {
                handleInput(code, false);
            }
        }
    }

    private void handleInput(final int code, boolean pressed) {
        if (code == -1) return;

        if (code >= 4000) { // TRIGGERS
            int axis = code - 4000;
            SDLControllerManager.onNativeJoy(virtualDeviceId, axis, pressed ? 1.0f : 0.0f);
        }
        else if (code >= 1000) {
            // D-PAD visuals only (Logic remains in updateDpadState)
        }
        else { // STANDARD BUTTONS
            if (pressed) {
                SDLControllerManager.onNativeHat(virtualDeviceId, 0, hatX, hatY);
                int result = SDLControllerManager.onNativePadDown(virtualDeviceId, code);
                if (result < 0) {
                    android.util.Log.e("SDL", "INPUT FAILURE: Device " + virtualDeviceId + " rejected button " + code);
                }
            } else {
                SDLControllerManager.onNativePadUp(virtualDeviceId, code);
            }
        }

        // Always update visuals
        updateButtonVisual(code, pressed);
    }

    private void updateButtonVisual(int code, boolean pressed) {
        int viewId = -1;
        switch (code) {
            case 96:  viewId = R.id.btn_a; break;
            case 97:  viewId = R.id.btn_b; break;
            case 99:  viewId = R.id.btn_x; break;
            case 100: viewId = R.id.btn_y; break;
            case 102: viewId = R.id.btn_d; break;
            case 103: viewId = R.id.btn_z; break;
            case 108: viewId = R.id.btn_start; break;
            case 109: viewId = R.id.btn_back; break;
            case 1001: viewId = R.id.dp_up; break;
            case 1002: viewId = R.id.dp_down; break;
            case 1003: viewId = R.id.dp_left; break;
            case 1004: viewId = R.id.dp_right; break;
//            case 1005: viewId = R.id.dp_upleft; break;
//            case 1006: viewId = R.id.dp_downright; break;
//            case 1007: viewId = R.id.dp_downleft; break;
//            case 1008: viewId = R.id.dp_upright; break;
            case 4004: viewId = R.id.btn_w; break;
            case 4005: viewId = R.id.btn_c; break;
        }

        if (viewId != -1) {
            View v = findViewById(viewId);
            if (v != null) v.setPressed(pressed);
        }
    }

    private void updateDpadState(MotionEvent event) {
        int newX = 0, newY = 0;
        int action = event.getActionMasked();

        if (action != MotionEvent.ACTION_UP && action != MotionEvent.ACTION_CANCEL) {
            View container = findViewById(R.id.dpad_container);
            if (container == null) return;

            // Use Local coordinates to match event.getX()
            float centerX = container.getLeft() + (container.getWidth() / 2f);
            float centerY = container.getTop() + (container.getHeight() / 2f);
            float radius = container.getWidth() / 2f;

            for (int i = 0; i < event.getPointerCount(); i++) {
                if (action == MotionEvent.ACTION_POINTER_UP && i == event.getActionIndex()) continue;

                float x = event.getX(i);
                float y = event.getY(i);

                // Calculate distance from center (Pythagoras)
                float dx = x - centerX;
                float dy = y - centerY;
                float distance = (float) Math.sqrt(dx * dx + dy * dy);

                // Only process if the finger is actually touching the D-Pad area
                // (with  a small "deadzone" in the middle so neutral is easy to hit)
                if (distance > (radius * 0.2f) && distance < (radius * 1.5f)) {
                    // Calculate angle in degrees (0 to 360)
                    double angle = Math.toDegrees(Math.atan2(dy, dx));
                    if (angle < 0) angle += 360;

                    // Map angle to 8 directions with wider diagonals
                    // Cardinals get 30 degrees, Diagonals get 60 degrees
                    if (angle >= 345 || angle < 15)        newX = 1;  // Right (Narrower)
                    else if (angle >= 15  && angle < 75)   { newX = 1; newY = 1; }   // Down-Right (Wider)
                    else if (angle >= 75  && angle < 105)  newY = 1; // Down
                    else if (angle >= 105 && angle < 165)  { newX = -1; newY = 1; }  // Down-Left
                    else if (angle >= 165 && angle < 195)  newX = -1; // Left
                    else if (angle >= 195 && angle < 255)  { newX = -1; newY = -1; } // Up-Left
                    else if (angle >= 255 && angle < 285)  newY = -1; // Up
                    else if (angle >= 285 && angle < 345)  { newX = 1; newY = -1; }  // Up-Right

                    // Map angle to 8 directions
//                    if (angle >= 337.5 || angle < 22.5)   newX = 1;  // Right
//                    else if (angle >= 22.5  && angle < 67.5)  { newX = 1; newY = 1; }   // Down-Right
//                    else if (angle >= 67.5  && angle < 112.5) newY = 1; // Down
//                    else if (angle >= 112.5 && angle < 157.5) { newX = -1; newY = 1; }  // Down-Left
//                    else if (angle >= 157.5 && angle < 202.5) newX = -1; // Left
//                    else if (angle >= 202.5 && angle < 247.5) { newX = -1; newY = -1; } // Up-Left
//                    else if (angle >= 247.5 && angle < 292.5) newY = -1; // Up
//                    else if (angle >= 292.5 && angle < 337.5) { newX = 1; newY = -1; }  // Up-Right

                    break; // One finger is enough for the D-pad
                }
            }
        }

        if (newX != hatX || newY != hatY) {
            hatX = newX;
            hatY = newY;
            SDLControllerManager.onNativeHat(virtualDeviceId, 0, hatX, hatY);
            updateDpadVisual(hatX, hatY);
        }
    }

    private int dpToPx(int dp) {
        float density = getContext().getResources().getDisplayMetrics().density;
        return Math.round((float) dp * density);
    }

    private Set<Integer> getButtonsAt(float x, float y) {
        Set<Integer> detectedButtons = new HashSet<>();

        // Define a "padding" factor in pixels. This allows the thumb to trigger
        // two buttons if it's in the gap between them.
        int padding = dpToPx(16);

        checkAndAdd(detectedButtons, R.id.btn_a, 96, x, y, padding);
        checkAndAdd(detectedButtons, R.id.btn_b, 97, x, y, padding);
        checkAndAdd(detectedButtons, R.id.btn_x, 99, x, y, padding);
        checkAndAdd(detectedButtons, R.id.btn_y, 100, x, y, padding);
        checkAndAdd(detectedButtons, R.id.btn_d, 102, x,y, padding);
        checkAndAdd(detectedButtons, R.id.btn_z, 103, x,y, padding);
        checkAndAdd(detectedButtons, R.id.btn_w, 4004, x,y, padding);
        checkAndAdd(detectedButtons, R.id.btn_c, 4005, x,y, padding);
        checkAndAdd(detectedButtons, R.id.btn_start, 108, x,y, padding);
        checkAndAdd(detectedButtons, R.id.btn_back, 109, x,y, padding);

        return detectedButtons;
    }

    private void checkAndAdd(Set<Integer> set, int viewId, int code, float x, float y, int pad) {
        View v = findViewById(viewId);
        if (v == null) return;

        int[] loc = new int[2];
        int[] parentLoc = new int[2];
        v.getLocationOnScreen(loc);
        this.getLocationOnScreen(parentLoc);

        // Calculate coordinates with padding
        int left   = loc[0] - parentLoc[0] - pad;
        int top    = loc[1] - parentLoc[1] - pad;
        int right  = left + v.getWidth() + pad*2;
        int bottom = top + v.getHeight() + pad*2;

        // Add if in region + padding
        if (x >= left && x <= right && y >= top && y <= bottom) {
            set.add(code);
        }
    }

    private void updateDpadVisual(int hX, int hY) {
        findViewById(R.id.dp_up).setPressed(hY == -1);
        findViewById(R.id.dp_down).setPressed(hY == 1);
        findViewById(R.id.dp_left).setPressed(hX == -1);
        findViewById(R.id.dp_right).setPressed(hX == 1);
    }

    // Helper to check if a finger is inside a specific button's area
    private boolean isViewAtLocation(View v, float x, float y) {
        if (v == null || v.getVisibility() != View.VISIBLE) return false;

        int[] vLoc = new int[2];
        v.getLocationOnScreen(vLoc); // Global position of button

        int[] parentLoc = new int[2];
        this.getLocationOnScreen(parentLoc); // Global position of the overlay

        // Get the position of the button RELATIVE to the overlay
        int relativeLeft = vLoc[0] - parentLoc[0];
        int relativeTop = vLoc[1] - parentLoc[1];

        return x >= relativeLeft && x <= (relativeLeft + v.getWidth()) &&
                y >= relativeTop && y <= (relativeTop + v.getHeight());
    }

    public ControllerOverlay(Context context) {
        super(context);
        this.setClickable(true);
        this.setFocusable(true);
        this.setEnabled(true);
        this.setMotionEventSplittingEnabled(true);

        // Inflate the buttons
        LayoutInflater inflater = LayoutInflater.from(context);
        View vc = inflater.inflate(R.layout.virtual_controller, this, true);
        disableAllTouches(vc);
    }

    private void disableAllTouches(View v) {
        v.setClickable(false);
        v.setFocusable(false);
        if (v instanceof ViewGroup) {
            ViewGroup vg = (ViewGroup) v;
            for (int i = 0; i < vg.getChildCount(); i++) {
                disableAllTouches(vg.getChildAt(i));
            }
        }
    }

    public void initializeVirtualController() {
        android.util.Log.i("ControllerOverlay", "DEBUG: initializing virtual joystick...");
        pointerStates.clear();
        hatX = 0;
        hatY = 0;
        android.util.Log.i("ControllerOverlay", String.format("DEBUG: virtualDeviceID = %d", virtualDeviceId));
        SDLControllerManager.nativeRemoveJoystick(virtualDeviceId);
        ensureJoystickAlive();
        // Initialize the joysticks
        JoystickOverlay ls = findViewById(R.id.left_analog);
        ls.setAttrs(virtualDeviceId, 0, 1);

        JoystickOverlay rs = findViewById(R.id.right_analog);
        rs.setAttrs(virtualDeviceId, 2, 3);

        for (int i = 0; i < 6; i++) {
            SDLControllerManager.onNativeJoy(virtualDeviceId, i, 0.00390625f);
            SDLControllerManager.onNativeJoy(virtualDeviceId, i, 0.0f);
        }
        SDLControllerManager.onNativeHat(virtualDeviceId, 0, 0, 0);
        android.util.Log.i("ControllerOverlay", "DEBUG: virtual joystick initialized!");
        isInitialized = true;
    }

    private int ensureJoystickAlive() {
        return SDLControllerManager.nativeAddJoystick(virtualDeviceId, DEVICE_NAME, DEVICE_NAME, 0, 0, false, 0xFFF, 6, 0x3F, 1, 0);
    }

    /**
     * Shows or hides the buttons. Hidden, their controller goes, so that a
     * real one takes its player; the next touch after they show again brings
     * it back.
     */
    public void setShown(boolean shown) {
        if (shown) {
            setVisibility(View.VISIBLE);
            return;
        }
        setVisibility(View.GONE);
        pointerStates.clear();
        leftJoyPointerId = -1;
        rightJoyPointerId = -1;
        hatX = 0;
        hatY = 0;
        if (isInitialized) {
            SDLControllerManager.nativeRemoveJoystick(virtualDeviceId);
            isInitialized = false;
        }
    }

    /**
     * Shows on each button the glyph image the game wrote for it in dir, or
     * its letter where it wrote none.
     */
    public void loadGlyphs(File dir) {
        for (int i = 0; i < GLYPH_NAMES.length; i++) {
            View v = findViewById(GLYPH_VIEWS[i]);
            if (!(v instanceof Button)) continue;
            Button button = (Button) v;
            if (!plainText.containsKey(GLYPH_VIEWS[i])) {
                plainText.put(GLYPH_VIEWS[i], button.getText());
                plainBackground.put(GLYPH_VIEWS[i], button.getBackground());
            }
            Drawable plain = plainBackground.get(GLYPH_VIEWS[i]);
            Bitmap glyph = BitmapFactory.decodeFile(new File(dir, GLYPH_NAMES[i] + ".png").getPath());
            if (glyph == null) {
                button.setText(plainText.get(GLYPH_VIEWS[i]));
                button.setBackground(plain);
            } else {
                button.setText("");
                Drawable image = new GlyphDrawable(glyph);
                button.setBackground(plain == null ? image : new LayerDrawable(new Drawable[] {plain, image}));
            }
        }
    }

    // A glyph fitted inside its bounds, a fifth of the way in, its pixels kept
    // sharp as the game draws them.
    static class GlyphDrawable extends Drawable {
        private final Bitmap bitmap;
        private final Paint paint = new Paint();
        private final Rect src;
        private final RectF dst = new RectF();

        GlyphDrawable(Bitmap bitmap) {
            this.bitmap = bitmap;
            src = new Rect(0, 0, bitmap.getWidth(), bitmap.getHeight());
            paint.setFilterBitmap(false);
        }

        @Override
        public void draw(Canvas canvas) {
            Rect b = getBounds();
            float inset = Math.min(b.width(), b.height()) * 0.2f;
            float scale = Math.min((b.width() - 2 * inset) / bitmap.getWidth(), (b.height() - 2 * inset) / bitmap.getHeight());
            float w = bitmap.getWidth() * scale / 2f, h = bitmap.getHeight() * scale / 2f;
            dst.set(b.exactCenterX() - w, b.exactCenterY() - h, b.exactCenterX() + w, b.exactCenterY() + h);
            canvas.drawBitmap(bitmap, src, dst, paint);
        }

        @Override
        public void setAlpha(int alpha) {
            paint.setAlpha(alpha);
            invalidateSelf();
        }

        @Override
        public void setColorFilter(ColorFilter colorFilter) {
            paint.setColorFilter(colorFilter);
            invalidateSelf();
        }

        @Override
        public int getOpacity() {
            return PixelFormat.TRANSLUCENT;
        }
    }
}
