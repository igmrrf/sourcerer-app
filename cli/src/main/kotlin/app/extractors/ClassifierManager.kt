// Copyright 2018 Sourcerer Inc. All Rights Reserved.
// Author: Liubov Yaronskaya (lyaronskaya@sourcerer.io)
// Author: Anatoly Kislov (anatoly@sourcerer.io)

package app.extractors

import app.BuildConfig
import app.Logger
import app.model.Classifier
import app.model.LibraryMeta
import app.utils.FileHelper
import org.apache.http.client.methods.HttpGet
import org.apache.http.impl.client.HttpClientBuilder
import java.io.FileOutputStream

class ClassifierManager {
    companion object {
        private const val CLASSIFIERS_DIR = "classifiers"
        private const val DATA_EXT = ".pb"
        private const val LIBS_META_DIR = ClassifierManager.CLASSIFIERS_DIR
        private const val LIBS_META_FILENAME = "libraries_meta.pb"
    }

    val cache = hashMapOf<String, Classifier>()
    val libsMeta = getLibraryMeta()

    /**
     * Returns libraries used in a line.
     */
    fun estimate(line: List<String>, libraries: List<String>): List<String> {
        return libraries.filter { libId ->
            if (!cache.containsKey(libId)) {
                // Library not downloaded from cloud storage.
                if (FileHelper.notExists(libId + DATA_EXT, CLASSIFIERS_DIR)) {
                    Logger.info { "Downloading $libId classifier" }
                    downloadClassifier(libId)
                    Logger.info { "Finished downloading $libId classifier" }
                }

                // Library not loaded from local storage.
                Logger.info { "Loading $libId evaluator" }
                loadClassifier(libId)
                Logger.info { "$libId evaluator ready" }
            }

            val classifier = cache[libId] ?: return@filter false

            // Check line for usage of a library.
            val prediction = classifier.evaluate(line)
            if (prediction.isEmpty()) return@filter false
            val libIndex = classifier.libraries.indexOf(libId)
            if (libIndex < 0 || libIndex >= prediction.size) return@filter false
            val prob = prediction[libIndex]
            // Define lower bound of classifier output
            // that depends on data used to create the model.
            // TODO(lyaronskaya): move thresholds to protobuf.
            if (libId == "rb.rails") {
                prob > 0.91
            } else if (libId == "py.flask") {
                prob > 0.95
            } else if (libId.startsWith(Lang.KOTLIN)) {
                prob > 0.8
            } else if (libId.startsWith(Lang.PLPGSQL)) {
                prob > 0.7
            } else if (libId.startsWith(Lang.PHP)) {
                prob > 0.75
            } else if (libId.startsWith(Lang.PYTHON)) {
                prob > 0.75
            } else if (libId.startsWith(Lang.SCALA)) {
                prob > 0.85
            } else if (libId.startsWith(Lang.GO)) {
                prob > 0.75
            } else if (libId.startsWith(Lang.JAVA)) {
                prob > 0.9
            } else if (libId.startsWith(Lang.CRYSTAL)) {
                prob > 0.7
            } else if (libId.startsWith(Lang.PERL)) {
                prob > 0.8
            } else if (libId.startsWith("rb.dry-")) {
                prob > 0.85
            } else if (libId == "js.q") {
                prob > 0.9
            } else if (libId == "cpp.gflags") {
                prob > 0.9
            } else if (libId == "dart.flutter") {
                prob > 0.85
            }
            else if (libId.startsWith(Lang.CPP)) {
                prob > 0.8
            }
            else if (libId.startsWith(Lang.JAVASCRIPT)) {
                prob > 0.7
            }
            else {
                prob > 0.5
            }
        }
    }

    /**
     * Downloads libraries from cloud.
     */
    private fun downloadClassifier(libId: String) {
        val file = FileHelper.getFile(libId + DATA_EXT, CLASSIFIERS_DIR)
        val langId = libId.split('.')[0]
        val url = "${BuildConfig.LIBRARY_MODELS_URL}$langId/$libId$DATA_EXT"
        val builder = HttpClientBuilder.create()
        val client = builder.build()
        try {
            client.execute(HttpGet(url)).use { response ->
                if (response.statusLine.statusCode == 200) {
                    val entity = response.entity
                    if (entity != null) {
                        FileOutputStream(file).use { outstream ->
                            entity.writeTo(outstream)
                            outstream.flush()
                            outstream.close()
                        }
                    }
                } else {
                    Logger.warn { "Classifier $libId unavailable from cloud (HTTP ${response.statusLine.statusCode})" }
                }
            }
        } catch (e: Exception) {
            Logger.error(e, "Failed to download $libId classifier")
        }
    }

    /**
     * Loads libraries from local storage to cache.
     */
    private fun loadClassifier(libId: String) {
        try {
            val file = FileHelper.getFile(libId + DATA_EXT, CLASSIFIERS_DIR)
            if (file.exists() && file.length() > 0) {
                val bytesArray = file.readBytes()
                cache[libId] = Classifier(bytesArray)
            }
        } catch (e: Exception) {
            Logger.warn { "Failed to load local classifier $libId: ${e.message}" }
        }
    }

    /**
     * Downloads libraries meta data from cloud.
     */
    private fun downloadLibrariesMeta() {
        val file = FileHelper.getFile(LIBS_META_FILENAME, LIBS_META_DIR)
        val url = BuildConfig.LIBRARY_MODELS_URL + LIBS_META_FILENAME
        val builder = HttpClientBuilder.create()
        val client = builder.build()
        try {
            client.execute(HttpGet(url)).use { response ->
                if (response.statusLine.statusCode == 200) {
                    val entity = response.entity
                    if (entity != null) {
                        FileOutputStream(file).use { outstream ->
                            entity.writeTo(outstream)
                            outstream.flush()
                            outstream.close()
                        }
                    }
                } else {
                    Logger.warn { "Libraries meta unavailable from cloud (HTTP ${response.statusLine.statusCode})" }
                }
            }
        } catch (e: Exception) {
            Logger.error(e, "Failed to download $LIBS_META_FILENAME")
        }
    }

    /**
     * Loads libraries meta data from local storage.
     */
    private fun getLibraryMeta(): LibraryMeta {
        return try {
            Logger.info { "Downloading $LIBS_META_FILENAME" }
            downloadLibrariesMeta()
            Logger.info { "Finished downloading $LIBS_META_FILENAME" }

            val file = FileHelper.getFile(LIBS_META_FILENAME, LIBS_META_DIR)
            if (file.exists() && file.length() > 0) {
                val bytesArray = file.readBytes()
                LibraryMeta(bytesArray)
            } else {
                LibraryMeta()
            }
        } catch (e: Throwable) {
            Logger.warn { "Falling back to empty library meta: ${e.message}" }
            LibraryMeta()
        }
    }
}
