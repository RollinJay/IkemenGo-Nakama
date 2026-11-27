package org.libsdl.app;

import android.content.SharedPreferences;

import java.util.Locale;

/**
 * The on-screen layouts the game chooses from (touchLayouts in
 * src/touch_glyphs.go, numbered alike), the arcade panels' coordinates, and
 * the player's own changes to a layout, kept per layout.
 *
 * The four panels are slagcoin.com's Japanese 8-button layouts, read from its
 * drawings: each button's centre in millimetres from the stick's, x to the
 * right and y down, the top row left to right and then the bottom row. The
 * d-pad stands for the stick. Every panel takes one scale, alike both ways,
 * at which the largest of them fits the screen; then the d-pad goes to the
 * left edge and the eight buttons, as one block keeping the drawing's shape,
 * to the right edge, both at the height the drawing gives them, so that the
 * middle of the game stays clear.
 */
final class TouchPanels {
    static final int CLASSIC = 1;
    static final int GRID = 2;
    static final int STRAIGHT = 3;
    static final int VEWLIX = 4;
    static final int ARCH = 5;
    static final int STAGGERED = 6;
    private static final String[] NAMES = {"classic", "grid", "straight", "vewlix", "arch", "staggered"};

    /** A panel's eight places: top row left to right, then bottom row. */
    static final int PLACES = 8;
    private static final float[][][] PANELS = {
            // Straight: two straight rows of four, 36 mm apart, the first
            // column 95 mm right of the stick, the rows 18 mm above and below
            // it (203 mm overall).
            {{95, -18}, {131, -18}, {167, -18}, {203, -18},
                    {95, 18}, {131, 18}, {167, 18}, {203, 18}},
            // Vewlix: the stick 95 mm left of the second column (59 + 36 mm),
            // the first column 31.25 mm left of that, then 35 and 36 mm; the
            // top row 9, 27, 36 and 36 mm above the stick, the bottom row 27
            // and 9 mm below it, then level (197.25 mm overall, from the
            // drawing's 95 mm reference left of the stick).
            {{63.75f, -9}, {95, -27}, {130, -36}, {166, -36},
                    {63.75f, 27}, {95, 9}, {130, 0}, {166, 0}},
            // Arch: the first column 95 mm right of the stick, then 31.25,
            // 35 and 35 mm; the second column highest (steps of 9, 9 and
            // 18 mm), the top row 18, 36, 27 and 18 mm above the stick and
            // the bottom row 18, 0, 9 and 18 mm below it (196.25 mm overall).
            {{95, -18}, {126.25f, -36}, {161.25f, -27}, {196.25f, -18},
                    {95, 18}, {126.25f, 0}, {161.25f, 9}, {196.25f, 18}},
            // Staggered: the bottom row 95, 33, 36 and 36 mm along, level
            // with the stick but for its first button 14 mm below; the top
            // row 7 mm further right, 38.5 mm above the stick but for its
            // first button 24.5 mm above (steps of 14, 10, 14.5 and 14 mm;
            // 207 mm overall).
            {{102, -24.5f}, {135, -38.5f}, {171, -38.5f}, {207, -38.5f},
                    {95, 14}, {128, 0}, {164, 0}, {200, 0}},
    };
    /** A panel's buttons are 30 mm across, as the drawings' are. */
    static final float BUTTON_MM = 30;
    /** The d-pad, in the same millimetres. */
    static final float DPAD_MM = 64;
    // The panel takes at most this share of the screen's height, from the
    // bottom, and its buttons and d-pad are at least this big, in dp.
    private static final float MAX_HEIGHT = 0.45f;
    private static final float MIN_BUTTON_DP = 48;
    private static final float MIN_DPAD_DP = 120;
    private static final float MARGIN_DP = 16;

    /**
     * Where each button sits by default (the game's [Touch Controls]
     * panel.top and panel.bottom): place p holds, in the 4 bits from 4p, one
     * more than the button's index in ControllerOverlay's REMOTE_VIEWS (a, b,
     * c, x, y, z, d, w), 0 for none. The default is x, y, z, w over a, b, c, d.
     */
    static final int DEFAULT_MAP = 4 | 5 << 4 | 6 << 8 | 8 << 12 | 1 << 16 | 2 << 20 | 3 << 24 | 7 << 28;

    private TouchPanels() {
    }

    /** The layout's name, as the game's and the preferences' keys spell it. */
    static String name(int layout) {
        return layout >= 1 && layout <= NAMES.length ? NAMES[layout - 1] : NAMES[0];
    }

    static boolean isPanel(int layout) {
        return layout >= STRAIGHT && layout <= STAGGERED;
    }

    /** The place (0 to 7) of grid button b (0 to 7) in map, or -1. */
    static int placeOf(int map, int b) {
        for (int p = 0; p < PLACES; p++) {
            if (((map >>> (4 * p)) & 0xF) == b + 1) {
                return p;
            }
        }
        return -1;
    }

    /**
     * Lays a panel out in a w by h area, density pixels to the dp: for each
     * place p, {centre x, centre y, diameter} in pixels, and the d-pad's at
     * index PLACES. The scale (scale()) is the same on every panel, so that
     * a button is the same size on each, and sizes the d-pad and the buttons
     * alike; a shorter panel just stands lower. The d-pad sits a margin from
     * the left edge and the button block a margin from the right, the
     * stick's row shared, the lower of them a margin from the bottom.
     */
    static float[][] place(int layout, float w, float h, float density) {
        float[][] panel = PANELS[layout - STRAIGHT];
        float rd = DPAD_MM / 2;
        // The rightmost and lowest button centres.
        float lastX = 0, lowY = -rd;
        for (float[] b : panel) {
            lastX = Math.max(lastX, b[0]);
            lowY = Math.max(lowY, b[1]);
        }
        float margin = MARGIN_DP * density;
        float s = scale(w, h, density);
        float button = Math.max(BUTTON_MM * s, MIN_BUTTON_DP * density);
        float dpad = Math.max(DPAD_MM * s, MIN_DPAD_DP * density);
        // The stick's row; the block's stick, from which its buttons are
        // measured; the d-pad's centre.
        float oy = h - margin - Math.max(dpad / 2, lowY * s + button / 2);
        float bx = w - margin - button / 2 - lastX * s;
        float dx = margin + dpad / 2;
        float[][] out = new float[PLACES + 1][];
        for (int p = 0; p < PLACES; p++) {
            out[p] = new float[] {bx + panel[p][0] * s, oy + panel[p][1] * s, button};
        }
        out[PLACES] = new float[] {dx, oy, dpad};
        return out;
    }

    /**
     * Pixels to the mm on every panel in a w by h area: the largest scale at
     * which each panel, as drawn, fits the width less margins and the bottom
     * MAX_HEIGHT of the height less a margin.
     */
    static float scale(float w, float h, float density) {
        float r = BUTTON_MM / 2, rd = DPAD_MM / 2;
        float margin = MARGIN_DP * density;
        float availW = w - 2 * margin, availH = h * MAX_HEIGHT - margin;
        float s = Float.MAX_VALUE;
        for (float[][] panel : PANELS) {
            float minX = -rd, maxX = rd, minY = -rd, maxY = rd;
            for (float[] b : panel) {
                minX = Math.min(minX, b[0] - r);
                maxX = Math.max(maxX, b[0] + r);
                minY = Math.min(minY, b[1] - r);
                maxY = Math.max(maxY, b[1] + r);
            }
            s = Math.min(s, Math.min(availW / (maxX - minX), availH / (maxY - minY)));
        }
        return Math.max(0, s);
    }

    /** The panel's coordinates in mm, for tests and the editor's reset. */
    static float[][] millimetres(int layout) {
        return PANELS[layout - STRAIGHT];
    }

    /**
     * The player's changes to one layout: for each of ControllerOverlay's
     * EDIT_NAMES, null or {x, y, size}, the element's centre as a share of
     * the on-screen buttons' width and height and its size as a multiple of
     * the layout's own, and the opacity of them all. Kept in the app's
     * preferences, per layout:
     * "touch_custom_<layout>" = "name:x,y,size;..." and
     * "touch_alpha_<layout>" = the opacity, from 0.2 to 1.
     */
    static final class Custom {
        static final float MIN_ALPHA = 0.2f;
        static final float MIN_SIZE = 0.5f;
        static final float MAX_SIZE = 2.5f;
        final float[][] pos;
        float alpha = 1f;

        Custom(int elements) {
            pos = new float[elements][];
        }

        Custom copy() {
            Custom c = new Custom(pos.length);
            for (int i = 0; i < pos.length; i++) {
                c.pos[i] = pos[i] == null ? null : pos[i].clone();
            }
            c.alpha = alpha;
            return c;
        }

        boolean isEmpty() {
            for (float[] p : pos) {
                if (p != null) return false;
            }
            return alpha >= 1f;
        }

        static String key(int layout) {
            return "touch_custom_" + name(layout);
        }

        static String alphaKey(int layout) {
            return "touch_alpha_" + name(layout);
        }

        static Custom load(SharedPreferences prefs, int layout, String[] names) {
            Custom c = new Custom(names.length);
            if (prefs == null) {
                return c;
            }
            c.alpha = clamp(prefs.getFloat(alphaKey(layout), 1f), MIN_ALPHA, 1f);
            String s = prefs.getString(key(layout), "");
            for (String entry : s.split(";")) {
                int colon = entry.indexOf(':');
                if (colon < 0) continue;
                String name = entry.substring(0, colon).trim();
                String[] v = entry.substring(colon + 1).split(",");
                if (v.length != 3) continue;
                for (int i = 0; i < names.length; i++) {
                    if (!names[i].equals(name)) continue;
                    try {
                        c.pos[i] = new float[] {
                                clamp(Float.parseFloat(v[0]), 0, 1),
                                clamp(Float.parseFloat(v[1]), 0, 1),
                                clamp(Float.parseFloat(v[2]), MIN_SIZE, MAX_SIZE)};
                    } catch (NumberFormatException ignored) {
                    }
                }
            }
            return c;
        }

        void save(SharedPreferences prefs, int layout, String[] names) {
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < pos.length; i++) {
                if (pos[i] == null) continue;
                if (sb.length() > 0) sb.append(';');
                sb.append(String.format(Locale.ROOT, "%s:%.4f,%.4f,%.3f", names[i], pos[i][0], pos[i][1], pos[i][2]));
            }
            SharedPreferences.Editor e = prefs.edit();
            if (sb.length() == 0) {
                e.remove(key(layout));
            } else {
                e.putString(key(layout), sb.toString());
            }
            if (alpha >= 1f) {
                e.remove(alphaKey(layout));
            } else {
                e.putFloat(alphaKey(layout), alpha);
            }
            e.apply();
        }
    }

    static float clamp(float v, float lo, float hi) {
        return Math.max(lo, Math.min(hi, v));
    }
}
