package org.libsdl.app;

import android.content.res.AssetManager;
import android.util.Log;

import java.io.*;
import java.util.HashMap;
import java.util.Map;
import java.util.zip.CRC32;
import java.util.zip.CheckedInputStream;

public class AssetExtractor {
    // In the game folder, the files extractAll wrote, with their CRCs.
    private static final String RECORD = "save/.ikemen-droid-assets";

    public static long getAssetCRC(AssetManager assets, String path) {
        try (InputStream is = assets.open(path);
             CheckedInputStream cis = new CheckedInputStream(is, new CRC32())) {
            byte[] buffer = new byte[16384];
            while (cis.read(buffer) >= 0) {}
            return cis.getChecksum().getValue();
        } catch (IOException e) {
            return -1; // File doesn't exist in APK
        }
    }

    public static long getFileCRC(File file) {
        if (!file.exists()) return -2;
        try (InputStream is = new FileInputStream(file);
             CheckedInputStream cis = new CheckedInputStream(is, new CRC32())) {
            byte[] buffer = new byte[16384];
            while (cis.read(buffer) >= 0) {}
            return cis.getChecksum().getValue();
        } catch (IOException e) {
            return -3;
        }
    }

    // Extracts the assets manifest.txt lists into targetDir. Those under
    // replaceDirs (the scripts) are always replaced. Any other file the folder
    // already has is replaced only if this app wrote it and it has not changed
    // since, so that a game's own files (its select.def, fight.def, glyphs,
    // server list, mods) never give way to the engine's defaults.
    public static void extractAll(AssetManager assets, File targetDir, String[] replaceDirs) throws IOException {
        Map<String, Long> written = readRecord(targetDir);
        Log.i("AssetExtractor", "Attempting to open manifest.txt...");
        InputStream manifestStream = null;
        try {
            manifestStream = assets.open("manifest.txt");
        } catch (IOException e) {
            Log.e("AssetExtractor", "CRITICAL: Could not find manifest.txt in assets root!", e);
            throw e; // Fail hard so you see it
        }

        BufferedReader reader = new BufferedReader(new InputStreamReader(manifestStream));
        String line;
        int count = 0;
        while ((line = reader.readLine()) != null) {
            line = line.trim();
            if (line.isEmpty()) continue;

            File outFile = new File(targetDir, line);
            count++;

            // Log every 10th file so we don't spam too hard but see progress
            if (count % 10 == 0) Log.d("AssetExtractor", "Extracting: " + line);

            String[] children = assets.list(line);
            boolean isDirectory = line.endsWith("/") || (children != null && children.length > 0);

            if (isDirectory) {
                // It's a directory (even if empty), just create it and move on
                if (!outFile.exists() && !outFile.mkdirs()) {
                    Log.e("AssetExtractor", "Failed to create directory: " + outFile.getAbsolutePath());
                }
                Log.d("AssetExtractor", "Created directory from manifest: " + line);
                continue;
            }

            if (outFile.exists() && !isUnder(line, replaceDirs)) {
                Long crc = written.get(line);
                if (crc == null || crc != getFileCRC(outFile)) {
                    continue; // The game's own file, or changed since: kept.
                }
            }

            File parentDir = outFile.getParentFile();
            if (parentDir != null && !parentDir.exists() && !parentDir.mkdirs()) {
                Log.e("AssetExtractor", "Failed to create parent dir: " + parentDir.getAbsolutePath());
            }

            try (InputStream in = assets.open(line);
                 OutputStream out = new FileOutputStream(outFile)) {

                if (in == null) throw new IOException("Could not open asset: " + line);

                byte[] buffer = new byte[16384];
                int read;
                long totalRead = 0;
                CRC32 crc = new CRC32();
                while ((read = in.read(buffer)) != -1) {
                    out.write(buffer, 0, read);
                    crc.update(buffer, 0, read);
                    totalRead += read;
                }
                out.flush(); // watch it, hardware
                written.put(line, crc.getValue());
                Log.d("AssetExtractor", "Wrote " + totalRead + " bytes to " + line);
            } catch (IOException e) {
                Log.e("AssetExtractor", "Failed to extract file: " + line, e);
            }
        }
        Log.i("AssetExtractor", "Extraction complete. Total files: " + count);
        reader.close();
        writeRecord(targetDir, written);
    }

    private static boolean isUnder(String path, String[] dirs) {
        for (String dir : dirs) {
            if (path.startsWith(dir + "/")) return true;
        }
        return false;
    }

    // Reads the record of the files extractAll wrote: a CRC, a tab and a path
    // on each line. A folder without one holds none of this app's files, as
    // far as it knows.
    private static Map<String, Long> readRecord(File targetDir) {
        Map<String, Long> record = new HashMap<>();
        try (BufferedReader r = new BufferedReader(new FileReader(new File(targetDir, RECORD)))) {
            String line;
            while ((line = r.readLine()) != null) {
                int tab = line.indexOf('\t');
                if (tab <= 0) continue;
                try {
                    record.put(line.substring(tab + 1), Long.parseLong(line.substring(0, tab)));
                } catch (NumberFormatException ignored) {
                }
            }
        } catch (IOException ignored) {
        }
        return record;
    }

    private static void writeRecord(File targetDir, Map<String, Long> record) {
        File f = new File(targetDir, RECORD);
        File parent = f.getParentFile();
        if (parent != null && !parent.exists() && !parent.mkdirs()) {
            Log.e("AssetExtractor", "Failed to create dir: " + parent.getAbsolutePath());
        }
        try (Writer w = new BufferedWriter(new FileWriter(f))) {
            for (Map.Entry<String, Long> e : record.entrySet()) {
                w.write(e.getValue() + "\t" + e.getKey() + "\n");
            }
        } catch (IOException e) {
            Log.e("AssetExtractor", "Failed to write " + f.getAbsolutePath(), e);
        }
    }
}
