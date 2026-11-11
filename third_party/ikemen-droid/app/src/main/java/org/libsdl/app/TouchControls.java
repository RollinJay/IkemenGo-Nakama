package org.libsdl.app;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Context;
import android.content.SharedPreferences;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.Rect;
import android.graphics.RectF;
import android.hardware.input.InputManager;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.util.TypedValue;
import android.view.InputDevice;
import android.view.KeyEvent;
import android.view.View;
import android.view.ViewGroup;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.RelativeLayout;

import org.ikemen_engine.ikemen_go.R;

import java.io.File;
import java.util.ArrayList;
import java.util.List;

/**
 * Decides whether the on-screen buttons show, from the player's setting
 * (automatic, always shown or always hidden) and the controllers connected,
 * and keeps the settings and keyboard buttons that go with them. The game
 * tells it when a text box opens or closes, and when it wrote the buttons'
 * glyph images.
 *
 * A TV remote is never a controller: as in other Android apps, its arrows
 * move a highlight between the buttons on the screen, and OK presses the
 * highlighted one.
 */
class TouchControls implements InputManager.InputDeviceListener {
    static final int MODE_AUTO = 0;
    static final int MODE_SHOW = 1;
    static final int MODE_HIDE = 2;
    private static final String PREF_MODE = "touch_controls";
    // A press from the remote lasts at least this long, so that the game,
    // which reads its controllers once a frame, sees it.
    private static final long MIN_PRESS_MS = 60;

    private final Activity activity;
    private final SharedPreferences prefs;
    private final ControllerOverlay overlay;
    private final ImageButton settingsButton;
    private final ImageButton keyboardButton;
    private final List<View> otherButtons = new ArrayList<>();
    private final FocusRing ring;
    private final Runnable showControls;
    private final InputManager inputManager;
    private final Handler handler = new Handler(Looper.getMainLooper());
    private boolean textEntry;
    private boolean keyboardClosed;
    // The remote's highlight: none until a remote key, nor after a touch.
    private View selected;
    // The on-screen button OK holds down, since when, and its release while
    // it waits for the press to last MIN_PRESS_MS.
    private View pressed;
    private long pressedAt;
    private Runnable pendingRelease;
    // Any other button, which OK clicks when it comes up, as Android's do.
    private View clicking;

    /**
     * showControls shows the faded controls again and restarts their fading,
     * as a touch does.
     */
    TouchControls(Activity activity, SharedPreferences prefs, ControllerOverlay overlay, ViewGroup layout, Runnable showControls) {
        this.activity = activity;
        this.prefs = prefs;
        this.overlay = overlay;
        this.showControls = showControls;

        settingsButton = addButton(layout, R.drawable.ic_touch_settings, R.string.touch_settings_button, 40,
                RelativeLayout.ALIGN_PARENT_LEFT, 0.5f);
        settingsButton.setOnClickListener(v -> showSettings());
        keyboardButton = addButton(layout, R.drawable.ic_keyboard, R.string.keyboard_button, 56,
                RelativeLayout.CENTER_HORIZONTAL, 0.7f);
        keyboardButton.setOnClickListener(v -> SDLActivity.nativeShowKeyboard());
        ring = new FocusRing(activity);
        ring.setZ(200f);
        layout.addView(ring, new RelativeLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        inputManager = (InputManager) activity.getSystemService(Context.INPUT_SERVICE);
        inputManager.registerInputDeviceListener(this, handler);
        reloadGlyphs();
        update();
    }

    // Adds a button with an icon at the top of the layout, over the
    // on-screen buttons (whose layer takes every touch otherwise). It never
    // takes the focus, which would take the keys from the game: the remote
    // reaches it through onRemoteKey.
    private ImageButton addButton(ViewGroup layout, int icon, int description, int sizeDp, int rule, float alpha) {
        ImageButton button = new ImageButton(activity);
        button.setImageResource(icon);
        button.setContentDescription(activity.getString(description));
        button.setBackgroundResource(R.drawable.round_button);
        button.setScaleType(ImageView.ScaleType.FIT_CENTER);
        button.setFocusable(false);
        int size = dp(sizeDp);
        button.setPadding(size / 5, size / 5, size / 5, size / 5);
        button.setAlpha(alpha);
        button.setZ(100f);
        RelativeLayout.LayoutParams params = new RelativeLayout.LayoutParams(size, size);
        params.addRule(RelativeLayout.ALIGN_PARENT_TOP);
        params.addRule(rule);
        params.setMargins(dp(8), dp(8), dp(8), dp(8));
        layout.addView(button, params);
        return button;
    }

    private int dp(int dp) {
        return Math.round(TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, dp, activity.getResources().getDisplayMetrics()));
    }

    /** Lets the remote click one of the activity's own buttons too. */
    void addRemoteButton(View button) {
        button.setFocusable(false);
        otherButtons.add(button);
    }

    int mode() {
        return prefs.getInt(PREF_MODE, MODE_AUTO);
    }

    /**
     * Shows or hides each part, for the setting, the controllers connected,
     * and whether a text box is open with its keyboard closed. Automatic hides
     * the on-screen buttons only while a real gamepad is connected; on a TV
     * they stay, for the remote.
     */
    void update() {
        int mode = mode();
        overlay.setShown(mode == MODE_SHOW || (mode == MODE_AUTO && ControllerOverlay.getPhysicalJoystickCount() == 0));
        boolean closed = textEntry && !SDLActivity.mScreenKeyboardShown;
        keyboardButton.setVisibility(closed ? View.VISIBLE : View.GONE);
        boolean justClosed = closed && !keyboardClosed;
        if (justClosed) {
            // A text box's keyboard just closed: the controls show, faded or
            // not, with the button that opens it again.
            showControls.run();
        }
        if (selected != null) {
            if (justClosed) {
                // The remote's highlight goes to that button.
                select(keyboardButton);
            } else if (!isTarget(selected)) {
                select(defaultTarget());
            }
        }
        keyboardClosed = closed;
        ring.invalidate();
    }

    /** A text box opened or closed in the game. */
    void setTextEntry(boolean open) {
        textEntry = open;
        update();
    }

    /** The game wrote the buttons' glyph images (src/touch_glyphs_android.go). */
    void reloadGlyphs() {
        overlay.loadGlyphs(new File(activity.getFilesDir(), "touch-glyphs"));
    }

    /**
     * Takes a TV remote's arrows and OK: they move the highlight and press the
     * highlighted button, and never reach the game themselves. Returns false
     * for every other key, Back among them, which goes on to the game.
     */
    boolean onRemoteKey(KeyEvent e) {
        int dx = 0, dy = 0;
        switch (e.getKeyCode()) {
            case KeyEvent.KEYCODE_DPAD_UP:
                dy = -1;
                break;
            case KeyEvent.KEYCODE_DPAD_DOWN:
                dy = 1;
                break;
            case KeyEvent.KEYCODE_DPAD_LEFT:
                dx = -1;
                break;
            case KeyEvent.KEYCODE_DPAD_RIGHT:
                dx = 1;
                break;
            case KeyEvent.KEYCODE_DPAD_CENTER:
            case KeyEvent.KEYCODE_ENTER:
            case KeyEvent.KEYCODE_NUMPAD_ENTER:
                break;
            default:
                return false;
        }
        if (!isRemote(e)) {
            return false;
        }
        int action = e.getAction();
        if (action != KeyEvent.ACTION_DOWN && action != KeyEvent.ACTION_UP) {
            return true;
        }
        boolean down = action == KeyEvent.ACTION_DOWN;
        showControls.run();
        if (selected == null || !isTarget(selected)) {
            // The first key shows where the highlight is.
            if (down && e.getRepeatCount() == 0) {
                select(defaultTarget());
            }
            return true;
        }
        if (dx != 0 || dy != 0) {
            // Held, an arrow repeats.
            if (down) {
                View next = nearest(selected, dx, dy);
                if (next != null) {
                    select(next);
                }
            }
        } else if (down) {
            if (e.getRepeatCount() == 0) {
                press(selected);
            }
        } else {
            unpress(!e.isCanceled());
        }
        return true;
    }

    /** A touch hides the remote's highlight, as Android's touch mode does. */
    void onTouch() {
        if (selected != null) {
            unpress(false);
            selected = null;
            ring.setTarget(null);
        }
    }

    /** Lets go of what the remote holds, as the app pauses. */
    void releaseAll() {
        unpress(false);
        flushRelease();
    }

    // A remote's keys come from a D-pad source, on a device that is neither
    // a gamepad, whose d-pad is the game's, nor a full keyboard, whose arrows
    // and Enter are the game's too. (A full keyboard's keys come from its
    // keyboard and D-pad sources alike; adb's "input dpad" from the D-pad's
    // alone.)
    private static boolean isRemote(KeyEvent e) {
        int source = e.getSource();
        if ((source & InputDevice.SOURCE_DPAD) != InputDevice.SOURCE_DPAD) {
            return false;
        }
        InputDevice device = e.getDevice();
        if (ControllerOverlay.isRealGamepad(device)) {
            return false;
        }
        boolean keyboard = (source & InputDevice.SOURCE_KEYBOARD) == InputDevice.SOURCE_KEYBOARD
                && device != null && device.getKeyboardType() == InputDevice.KEYBOARD_TYPE_ALPHABETIC;
        return !keyboard;
    }

    // The buttons the highlight can go to: the on-screen buttons shown, the
    // settings button, the keyboard button while it shows, and the activity's.
    private List<View> targets() {
        List<View> targets = overlay.navigationTargets();
        targets.add(settingsButton);
        if (keyboardButton.getVisibility() == View.VISIBLE) {
            targets.add(keyboardButton);
        }
        for (View v : otherButtons) {
            if (v.getVisibility() == View.VISIBLE) {
                targets.add(v);
            }
        }
        return targets;
    }

    private boolean isTarget(View v) {
        return targets().contains(v);
    }

    // The keyboard button while it shows, else Start, which opens the game's
    // menus.
    private View defaultTarget() {
        if (keyboardButton.getVisibility() == View.VISIBLE) {
            return keyboardButton;
        }
        for (View v : overlay.navigationTargets()) {
            if (v.getId() == R.id.btn_start) {
                return v;
            }
        }
        return settingsButton;
    }

    // The target to go to in direction dx, dy. A target in line with from
    // (overlapping it across the direction) wins, then one within 45 degrees
    // of the direction, then any other; among equals, the nearer wins, the
    // distance along the direction counting 13 times the distance across,
    // squared, as in Android's own focus search.
    private View nearest(View from, int dx, int dy) {
        Rect f = screenRect(from);
        View best = null;
        int bestRank = Integer.MAX_VALUE;
        long bestScore = Long.MAX_VALUE;
        for (View v : targets()) {
            if (v == from) {
                continue;
            }
            Rect c = screenRect(v);
            long along = (long) (c.centerX() - f.centerX()) * dx + (long) (c.centerY() - f.centerY()) * dy;
            if (along <= 0) {
                continue;
            }
            long across = dx != 0 ? Math.abs(c.centerY() - f.centerY()) : Math.abs(c.centerX() - f.centerX());
            boolean inLine = dx != 0 ? c.top < f.bottom && c.bottom > f.top : c.left < f.right && c.right > f.left;
            int rank = inLine ? 0 : across <= along ? 1 : 2;
            long score = 13 * along * along + across * across;
            if (rank < bestRank || rank == bestRank && score < bestScore) {
                best = v;
                bestRank = rank;
                bestScore = score;
            }
        }
        return best;
    }

    private static Rect screenRect(View v) {
        int[] loc = new int[2];
        v.getLocationOnScreen(loc);
        return new Rect(loc[0], loc[1], loc[0] + v.getWidth(), loc[1] + v.getHeight());
    }

    // Moves the highlight to v. On an on-screen button, it registers their
    // controller, if the buttons have none yet, so that the game has it by
    // the time OK presses one.
    private void select(View v) {
        unpress(false);
        selected = v;
        ring.setTarget(v);
        if (overlay.isButton(v)) {
            overlay.ensureController();
        }
    }

    // OK went down on v.
    private void press(View v) {
        flushRelease();
        if (overlay.isButton(v)) {
            pressed = v;
            pressedAt = SystemClock.uptimeMillis();
            overlay.pressTarget(v, true);
        } else {
            clicking = v;
            v.setPressed(true);
        }
    }

    // OK came up, clicking a button that waits for it if click is set, or the
    // highlight moved or went.
    private void unpress(boolean click) {
        View c = clicking;
        if (c != null) {
            clicking = null;
            c.setPressed(false);
            if (click) {
                c.performClick();
            }
        }
        View v = pressed;
        if (v == null) {
            return;
        }
        pressed = null;
        Runnable r = () -> {
            pendingRelease = null;
            overlay.pressTarget(v, false);
        };
        long wait = MIN_PRESS_MS - (SystemClock.uptimeMillis() - pressedAt);
        if (wait > 0) {
            pendingRelease = r;
            handler.postDelayed(r, wait);
        } else {
            r.run();
        }
    }

    // Sends a release that is still waiting now, before another press.
    private void flushRelease() {
        Runnable r = pendingRelease;
        if (r != null) {
            handler.removeCallbacks(r);
            r.run();
        }
    }

    private void showSettings() {
        String[] modes = {
                activity.getString(R.string.touch_controls_auto),
                activity.getString(R.string.touch_controls_show),
                activity.getString(R.string.touch_controls_hide),
        };
        new AlertDialog.Builder(activity)
                .setTitle(R.string.touch_controls_title)
                .setSingleChoiceItems(modes, mode(), (dialog, which) -> {
                    prefs.edit().putInt(PREF_MODE, which).apply();
                    update();
                    dialog.dismiss();
                })
                .show();
    }

    void close() {
        inputManager.unregisterInputDeviceListener(this);
        handler.removeCallbacksAndMessages(null);
    }

    @Override
    public void onInputDeviceAdded(int deviceId) {
        update();
    }

    @Override
    public void onInputDeviceRemoved(int deviceId) {
        update();
    }

    @Override
    public void onInputDeviceChanged(int deviceId) {
        update();
    }

    // The remote's highlight: a ring around the selected button, round on a
    // round button and a pill on a wide one.
    static class FocusRing extends View {
        private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final RectF rect = new RectF();
        private View target;

        FocusRing(Context context) {
            super(context);
            paint.setStyle(Paint.Style.STROKE);
            paint.setColor(0xFFFFD54F);
            setClickable(false);
            setFocusable(false);
            setWillNotDraw(false);
        }

        void setTarget(View target) {
            this.target = target;
            invalidate();
        }

        @Override
        protected void onDraw(Canvas canvas) {
            if (target == null || !target.isShown()) {
                return;
            }
            int[] me = new int[2], t = new int[2];
            getLocationOnScreen(me);
            target.getLocationOnScreen(t);
            float w = getResources().getDisplayMetrics().density * 3;
            paint.setStrokeWidth(w);
            float left = t[0] - me[0], top = t[1] - me[1];
            rect.set(left - w, top - w, left + target.getWidth() + w, top + target.getHeight() + w);
            float r = Math.min(rect.width(), rect.height()) / 2f;
            canvas.drawRoundRect(rect, r, r, paint);
        }
    }
}
