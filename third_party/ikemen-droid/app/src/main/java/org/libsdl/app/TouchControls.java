package org.libsdl.app;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Context;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.hardware.input.InputManager;
import android.os.Handler;
import android.os.Looper;
import android.util.TypedValue;
import android.view.View;
import android.view.ViewGroup;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.RelativeLayout;

import org.ikemen_engine.ikemen_go.R;

import java.io.File;

/**
 * Decides whether the on-screen buttons show, from the player's setting
 * (automatic, always shown or always hidden), Android TV and the controllers
 * connected, and keeps the settings and keyboard buttons that go with them.
 * The game tells it when a text box opens or closes, and when it wrote the
 * buttons' glyph images.
 */
class TouchControls implements InputManager.InputDeviceListener {
    static final int MODE_AUTO = 0;
    static final int MODE_SHOW = 1;
    static final int MODE_HIDE = 2;
    private static final String PREF_MODE = "touch_controls";

    private final Activity activity;
    private final SharedPreferences prefs;
    private final ControllerOverlay overlay;
    private final ImageButton settingsButton;
    private final ImageButton keyboardButton;
    private final InputManager inputManager;
    private final boolean touchscreen;
    private final boolean tv;
    private boolean textEntry;

    TouchControls(Activity activity, SharedPreferences prefs, ControllerOverlay overlay, ViewGroup layout) {
        this.activity = activity;
        this.prefs = prefs;
        this.overlay = overlay;
        PackageManager pm = activity.getPackageManager();
        touchscreen = pm.hasSystemFeature(PackageManager.FEATURE_TOUCHSCREEN);
        tv = SDLActivity.isAndroidTV() || pm.hasSystemFeature(PackageManager.FEATURE_LEANBACK);

        settingsButton = addButton(layout, R.drawable.ic_touch_settings, R.string.touch_settings_button, 40,
                RelativeLayout.ALIGN_PARENT_LEFT, 0.5f);
        settingsButton.setOnClickListener(v -> showSettings());
        keyboardButton = addButton(layout, R.drawable.ic_keyboard, R.string.keyboard_button, 56,
                RelativeLayout.CENTER_HORIZONTAL, 0.7f);
        keyboardButton.setOnClickListener(v -> SDLActivity.nativeShowKeyboard());

        inputManager = (InputManager) activity.getSystemService(Context.INPUT_SERVICE);
        inputManager.registerInputDeviceListener(this, new Handler(Looper.getMainLooper()));
        reloadGlyphs();
        update();
    }

    // Adds a button with an icon at the top of the layout, over the
    // on-screen buttons (whose layer takes every touch otherwise).
    private ImageButton addButton(ViewGroup layout, int icon, int description, int sizeDp, int rule, float alpha) {
        ImageButton button = new ImageButton(activity);
        button.setImageResource(icon);
        button.setContentDescription(activity.getString(description));
        button.setBackgroundResource(R.drawable.round_button);
        button.setScaleType(ImageView.ScaleType.FIT_CENTER);
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

    int mode() {
        return prefs.getInt(PREF_MODE, MODE_AUTO);
    }

    /** Shows or hides each part, for the setting and what is connected now. */
    void update() {
        int mode = mode();
        boolean automatic = touchscreen && !tv && ControllerOverlay.getPhysicalJoystickCount() == 0;
        overlay.setShown(mode == MODE_SHOW || (mode == MODE_AUTO && automatic));
        settingsButton.setVisibility(touchscreen ? View.VISIBLE : View.GONE);
        keyboardButton.setVisibility(touchscreen && textEntry ? View.VISIBLE : View.GONE);
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
}
