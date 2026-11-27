package org.libsdl.app;

import android.content.Context;
import android.graphics.Canvas;
import android.graphics.DashPathEffect;
import android.graphics.Paint;
import android.graphics.RectF;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.MotionEvent;
import android.view.ScaleGestureDetector;
import android.view.View;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;

import org.ikemen_engine.ikemen_go.R;

import java.util.Locale;

/**
 * The player's editor of the on-screen buttons, over them: a finger drags
 * any button, the d-pad, Start or Menu; two fingers pinch the one last
 * touched bigger or smaller, as do the Smaller and Bigger buttons; Opacity
 * fades them all. Save keeps the changes for the current layout (each layout
 * keeps its own, TouchPanels.Custom), Reset goes back to the layout's own
 * places, and Cancel, or Back, leaves without saving. The buttons play
 * nothing while it is open. Every element stays whole on the screen, and the
 * bar of actions moves between the top and the bottom to keep off the one
 * being moved, and off as many others as it can.
 */
class TouchLayoutEditor extends FrameLayout {
    private static final float[] ALPHAS = {1f, 0.8f, 0.6f, 0.4f};
    private final ControllerOverlay overlay;
    private final Runnable onClose;
    private final TouchPanels.Custom saved;
    private TouchPanels.Custom working;
    private final ScaleGestureDetector pinch;
    private final Paint outline = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint highlight = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final TextView title;
    private final Button opacity;
    private final LinearLayout bar;
    private boolean barAtBottom;
    // The element being dragged (an index in ControllerOverlay.EDIT_VIEWS),
    // the finger's offset from its centre, and the one last touched.
    private int dragging = -1;
    private int dragPointer = -1;
    private float offsetX, offsetY;
    private int selected = -1;

    TouchLayoutEditor(Context context, ControllerOverlay overlay, Runnable onClose) {
        super(context);
        this.overlay = overlay;
        this.onClose = onClose;
        saved = overlay.custom().copy();
        working = saved.copy();
        setWillNotDraw(false);
        setClickable(true);
        setBackgroundColor(0x33000000);

        float d = getResources().getDisplayMetrics().density;
        outline.setStyle(Paint.Style.STROKE);
        outline.setStrokeWidth(2 * d);
        outline.setColor(0xCCFFFFFF);
        outline.setPathEffect(new DashPathEffect(new float[] {8 * d, 6 * d}, 0));
        highlight.setStyle(Paint.Style.STROKE);
        highlight.setStrokeWidth(3 * d);
        highlight.setColor(0xFFFFD54F);

        bar = new LinearLayout(context);
        bar.setOrientation(LinearLayout.VERTICAL);
        bar.setGravity(Gravity.CENTER_HORIZONTAL);
        bar.setBackgroundColor(0xCC202020);
        int pad = dp(6);
        bar.setPadding(pad, pad, pad, pad);
        title = new TextView(context);
        title.setTextColor(0xFFFFFFFF);
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        title.setGravity(Gravity.CENTER);
        bar.addView(title);
        LinearLayout row = new LinearLayout(context);
        row.setOrientation(LinearLayout.HORIZONTAL);
        bar.addView(row);
        addAction(row, R.string.touch_edit_smaller, v -> resizeSelected(1 / 1.15f));
        addAction(row, R.string.touch_edit_bigger, v -> resizeSelected(1.15f));
        opacity = addAction(row, R.string.touch_edit_opacity, v -> nextOpacity());
        addAction(row, R.string.touch_edit_reset, v -> reset());
        addAction(row, R.string.touch_edit_cancel, v -> cancel());
        addAction(row, R.string.touch_edit_save, v -> save());
        FrameLayout.LayoutParams lp = new FrameLayout.LayoutParams(LayoutParams.WRAP_CONTENT, LayoutParams.WRAP_CONTENT,
                Gravity.TOP | Gravity.CENTER_HORIZONTAL);
        lp.topMargin = overlay.getPaddingTop() + dp(4);
        addView(bar, lp);

        pinch = new ScaleGestureDetector(context, new ScaleGestureDetector.SimpleOnScaleGestureListener() {
            @Override
            public boolean onScale(ScaleGestureDetector detector) {
                resizeSelected(detector.getScaleFactor());
                return true;
            }
        });
        overlay.setCustom(working);
        updateTitle();
        // Its size is known once it is laid out (and changes with the title).
        bar.addOnLayoutChangeListener((v, l, t, r, b, ol, ot, or, ob) -> post(this::placeBar));
    }

    @Override
    protected void onSizeChanged(int w, int h, int oldw, int oldh) {
        super.onSizeChanged(w, h, oldw, oldh);
        post(this::placeBar);
    }

    // The bar's rectangle at the top or the bottom, in the overlay's
    // coordinates (this view lies over it, at the same place).
    private RectF barRect(boolean bottom) {
        float bw = bar.getWidth(), bh = bar.getHeight();
        float left = (getWidth() - bw) / 2f;
        float top = bottom ? getHeight() - overlay.getPaddingBottom() - dp(4) - bh : overlay.getPaddingTop() + dp(4);
        return new RectF(left, top, left + bw, top + bh);
    }

    // How much of the shown elements the bar would cover there, the one
    // selected counting above all the others together.
    private float covered(RectF bar) {
        float sum = 0;
        for (int i = 0; i < ControllerOverlay.EDIT_VIEWS.length; i++) {
            RectF r = overlay.elementRect(i);
            if (r == null) continue;
            RectF o = new RectF();
            if (!o.setIntersect(r, bar)) continue;
            float area = o.width() * o.height();
            sum += i == selected ? area * 1000f : area;
        }
        return sum;
    }

    // Moves the bar to the top or the bottom, whichever covers less of the
    // elements; it stays put on a tie.
    private void placeBar() {
        if (bar.getWidth() == 0 || getHeight() == 0) return;
        float here = covered(barRect(barAtBottom)), there = covered(barRect(!barAtBottom));
        if (there >= here) return;
        barAtBottom = !barAtBottom;
        FrameLayout.LayoutParams lp = (FrameLayout.LayoutParams) bar.getLayoutParams();
        lp.gravity = (barAtBottom ? Gravity.BOTTOM : Gravity.TOP) | Gravity.CENTER_HORIZONTAL;
        lp.topMargin = barAtBottom ? 0 : overlay.getPaddingTop() + dp(4);
        lp.bottomMargin = barAtBottom ? overlay.getPaddingBottom() + dp(4) : 0;
        bar.setLayoutParams(lp);
    }

    private Button addAction(LinearLayout row, int text, View.OnClickListener listener) {
        Button b = new Button(getContext());
        b.setText(text);
        b.setAllCaps(false);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        b.setMinWidth(0);
        b.setMinimumWidth(dp(56));
        b.setOnClickListener(listener);
        row.addView(b, new LinearLayout.LayoutParams(LayoutParams.WRAP_CONTENT, dp(44)));
        return b;
    }

    private int dp(int dp) {
        return Math.round(TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, dp, getResources().getDisplayMetrics()));
    }

    private void updateTitle() {
        String layout = TouchPanels.name(overlay.layout());
        String what = selected >= 0 ? ControllerOverlay.EDIT_NAMES[selected] : "-";
        title.setText(getContext().getString(R.string.touch_edit_title, layout, what));
        opacity.setText(String.format(Locale.ROOT, "%s %d%%", getContext().getString(R.string.touch_edit_opacity),
                Math.round(working.alpha * 100)));
    }

    @Override
    public boolean onTouchEvent(MotionEvent e) {
        pinch.onTouchEvent(e);
        int action = e.getActionMasked();
        int index = e.getActionIndex();
        // The overlay lies under this view, at the same place.
        switch (action) {
            case MotionEvent.ACTION_DOWN: {
                float x = e.getX(index), y = e.getY(index);
                int i = overlay.elementAt(x, y);
                if (i >= 0) {
                    RectF r = overlay.elementRect(i);
                    dragging = i;
                    dragPointer = e.getPointerId(index);
                    offsetX = x - r.centerX();
                    offsetY = y - r.centerY();
                    selected = i;
                    updateTitle();
                    placeBar();
                    invalidate();
                }
                return true;
            }
            case MotionEvent.ACTION_POINTER_DOWN:
                // A second finger pinches: no more dragging.
                dragging = -1;
                return true;
            case MotionEvent.ACTION_MOVE: {
                if (dragging < 0 || pinch.isInProgress()) return true;
                int pi = e.findPointerIndex(dragPointer);
                if (pi < 0) return true;
                moveTo(dragging, e.getX(pi) - offsetX, e.getY(pi) - offsetY);
                return true;
            }
            case MotionEvent.ACTION_UP:
            case MotionEvent.ACTION_CANCEL:
                dragging = -1;
                dragPointer = -1;
                return true;
        }
        return true;
    }

    // Puts element i's centre at x, y, kept whole on the screen.
    private void moveTo(int i, float x, float y) {
        float w = overlay.getWidth(), h = overlay.getHeight();
        if (w <= 0 || h <= 0) return;
        float[] p = ensureOwn(i);
        if (p == null) return;
        p[0] = TouchPanels.clamp(x / w, 0, 1);
        p[1] = TouchPanels.clamp(y / h, 0, 1);
        applyChange(i);
    }

    // Shows the working layout, keeps element i's place as the overlay
    // drew it (kept inside its padded area) and moves the bar off it.
    private void applyChange(int i) {
        overlay.setCustom(working);
        RectF r = overlay.elementRect(i);
        float[] p = working.pos[i];
        if (r != null && p != null) {
            p[0] = r.centerX() / overlay.getWidth();
            p[1] = r.centerY() / overlay.getHeight();
        }
        placeBar();
        invalidate();
    }

    // Element i's own place, starting from where the layout puts it.
    private float[] ensureOwn(int i) {
        if (working.pos[i] == null) {
            float[] place = overlay.layoutPlace(i);
            if (place == null) return null;
            working.pos[i] = new float[] {place[0] / overlay.getWidth(), place[1] / overlay.getHeight(), 1f};
        }
        return working.pos[i];
    }

    private void resizeSelected(float factor) {
        if (selected < 0) return;
        float[] p = ensureOwn(selected);
        if (p == null) return;
        p[2] = TouchPanels.clamp(p[2] * factor, TouchPanels.Custom.MIN_SIZE, TouchPanels.Custom.MAX_SIZE);
        applyChange(selected);
    }

    private void nextOpacity() {
        int next = 0;
        for (int i = 0; i < ALPHAS.length; i++) {
            if (Math.abs(ALPHAS[i] - working.alpha) < 0.01f) {
                next = (i + 1) % ALPHAS.length;
                break;
            }
        }
        working.alpha = ALPHAS[next];
        overlay.setCustom(working);
        updateTitle();
        invalidate();
    }

    /** Back to the layout's own places (kept once saved). */
    void reset() {
        working = new TouchPanels.Custom(ControllerOverlay.EDIT_VIEWS.length);
        selected = -1;
        overlay.setCustom(working);
        updateTitle();
        placeBar();
        invalidate();
    }

    void cancel() {
        overlay.setCustom(saved);
        onClose.run();
    }

    void save() {
        overlay.setCustom(working);
        overlay.saveCustom();
        onClose.run();
    }

    @Override
    protected void onDraw(Canvas canvas) {
        super.onDraw(canvas);
        for (int i = 0; i < ControllerOverlay.EDIT_VIEWS.length; i++) {
            RectF r = overlay.elementRect(i);
            if (r == null) continue;
            float rad = Math.min(r.width(), r.height()) / 2f;
            canvas.drawRoundRect(r, rad, rad, i == selected ? highlight : outline);
        }
    }
}
