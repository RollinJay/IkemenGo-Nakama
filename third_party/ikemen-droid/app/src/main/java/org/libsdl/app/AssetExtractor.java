package org.libsdl.app;

import android.content.res.AssetManager;
import android.util.Log;

import java.io.*;
import java.util.zip.CRC32;
import java.util.zip.CheckedInputStream;

public class AssetExtractor {

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

    // Extract all assets in assets/ into baseDir
    public static void extractAll(AssetManager assets, File targetDir) throws IOException {
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
                while ((read = in.read(buffer)) != -1) {
                    out.write(buffer, 0, read);
                    totalRead += read;
                }
                out.flush(); // watch it, hardware
                Log.d("AssetExtractor", "Wrote " + totalRead + " bytes to " + line);
            } catch (IOException e) {
                Log.e("AssetExtractor", "Failed to extract file: " + line, e);
            }
        }
        Log.i("AssetExtractor", "Extraction complete. Total files: " + count);
        reader.close();
    }
}
