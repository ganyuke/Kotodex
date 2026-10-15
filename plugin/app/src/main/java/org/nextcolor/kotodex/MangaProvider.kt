package org.nextcolor.kotodex

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.util.Locale

/**
 * The manga source Kotatsu sees. Kotatsu finds this provider by its
 * app.kotatsu.parser.PROVIDE_MANGA intent filter and queries these paths
 * (see ExternalPluginContentSource in Kotatsu):
 *
 *   manga                  list, with offset, query and tags_include parameters
 *   manga/<id>             details
 *   manga/chapters/<id>    chapters
 *   chapters/<id>          pages of a chapter
 *   manga/pages/0?url=     full image URL for a page
 *   filter/<kind>          filter options
 *   capabilities           sort orders and filter support
 *
 * Errors reach Kotatsu as IllegalStateException, one of the few exception
 * types that cross process boundaries with their message.
 *
 * The provider has to be exported for Kotatsu to find it, so it checks who
 * is asking. Kotatsu and Futon are always answered. Any other app is refused
 * until the user allows it in the Kotodex app, which covers other Kotatsu forks.
 */
class MangaProvider : ContentProvider() {

    private lateinit var prefs: Prefs

    override fun onCreate(): Boolean {
        prefs = Prefs(context!!)
        return true
    }

    override fun query(
        uri: Uri,
        projection: Array<out String>?,
        selection: String?,
        selectionArgs: Array<out String>?,
        sortOrder: String?,
    ): Cursor {
        checkCaller()
        val seg = uri.pathSegments
        if (seg == listOf("capabilities")) return capabilities()
        if (seg.size == 2 && seg[0] == "filter") return filter(seg[1])
        if (seg.size == 3 && seg[0] == "manga" && seg[1] == "pages") {
            return MatrixCursor(arrayOf("value")).apply { addRow(arrayOf(uri.getQueryParameter("url"))) }
        }
        val api = api()
        return call {
            when {
                seg == listOf("manga") -> list(api, uri, sortOrder)
                seg.size == 3 && seg[0] == "manga" && seg[1] == "chapters" -> chapters(api.manga(seg[2]))
                seg.size == 2 && seg[0] == "manga" -> mangaCursor(api).apply { addManga(api, api.manga(seg[1])) }
                seg.size == 2 && seg[0] == "chapters" -> pages(api, seg[1])
                else -> throw IllegalArgumentException("Unknown path ${uri.path}")
            }
        }
    }

    private fun checkCaller() {
        val caller = callingPackage ?: throw SecurityException("Kotodex couldn't tell which app is asking")
        if (caller == context!!.packageName || caller in READER_PACKAGES || caller in prefs.allowedApps) return
        prefs.request(caller)
        throw SecurityException("Open the Kotodex app and allow $caller")
    }

    private fun api(): Api {
        if (!prefs.signedIn) throw IllegalStateException("Open the Kotodex app and sign in to your server")
        return Api(prefs.server, prefs.token)
    }

    private inline fun <T> call(block: () -> T): T = try {
        block()
    } catch (e: ApiException) {
        if (e.status == 401) {
            throw IllegalStateException("Signed out of Kotodex. Open the Kotodex app and sign in again.")
        }
        throw IllegalStateException(e.message)
    } catch (e: IOException) {
        throw IllegalStateException("Can't reach the Kotodex server: ${e.message}")
    }

    private fun list(api: Api, uri: Uri, sortOrder: String?): Cursor {
        val sort = when (sortOrder) {
            "ALPHABETICAL" -> "title"
            "NEWEST" -> "added"
            else -> "updated"
        }
        val tags = uri.getQueryParameters("tags_include").map { it.substringBefore('=') }
        val items = api.list(
            offset = uri.getQueryParameter("offset")?.toIntOrNull() ?: 0,
            sort = sort,
            query = uri.getQueryParameter("query"),
            tags = tags,
            followedOnly = prefs.followedOnly,
        )
        val cursor = mangaCursor(api)
        for (i in 0 until items.length()) cursor.addManga(api, items.getJSONObject(i))
        return cursor
    }

    private fun mangaCursor(api: Api) = MatrixCursor(MANGA_COLUMNS)

    private fun MatrixCursor.addManga(api: Api, m: JSONObject) {
        val id = m.getString("id")
        val alt = m.optJSONArray("altTitles")?.optString(0)?.takeIf { it.isNotEmpty() }
        val tags = m.optJSONArray("tags").strings().joinToString(":") { val t = tagKey(it); "$t=$t" }
        val authors = m.optJSONArray("authors").strings().joinToString(",")
        val rating = m.optString("contentRating")
        addRow(
            arrayOf<Any?>(
                stableId("manga:$id"),
                m.getString("title"),
                alt,
                id,
                m.optString("publicUrl"),
                -1f,
                if (rating == "erotica" || rating == "pornographic") 1 else 0,
                m.optString("coverUrl").takeIf { it.isNotEmpty() }?.let(api::absolute),
                tags.ifEmpty { null },
                state(m.optString("status")),
                authors.ifEmpty { null },
                m.optString("largeCoverUrl").takeIf { it.isNotEmpty() }?.let(api::absolute),
                m.optString("description").ifEmpty { null },
            ),
        )
    }

    /**
     * Chapters are grouped into branches by language, like Kotatsu's own
     * MangaDex source. A second upload of the same chapter by another group
     * goes to "English (1)" and so on, so each branch reads straight through.
     */
    private fun chapters(m: JSONObject): Cursor {
        val list = m.optJSONArray("chapterList") ?: JSONArray()
        val cursor = MatrixCursor(CHAPTER_COLUMNS)
        val taken = HashMap<String, HashSet<Pair<Int, Float>>>()
        for (i in 0 until list.length()) {
            val c = list.getJSONObject(i)
            val numbered = !c.isNull("number")
            val number = c.optDouble("number", 0.0).toFloat()
            val volume = c.optInt("volume", 0)
            val locale = Locale.forLanguageTag(languageTag(c.optString("lang")))
            val lang = locale.getDisplayName(locale).replaceFirstChar { it.titlecase(locale) }
            val key = volume to number
            // Chapters without a number can't be the same chapter twice.
            val branch = if (!numbered) {
                lang
            } else {
                generateSequence(0) { it + 1 }
                    .map { if (it == 0) lang else "$lang ($it)" }
                    .first { taken[it]?.contains(key) != true }
                    .also { taken.getOrPut(it, ::HashSet).add(key) }
            }
            val chapterId = c.getString("id")
            cursor.addRow(
                arrayOf<Any?>(
                    stableId("chapter:$chapterId"),
                    c.optString("title").ifEmpty { null },
                    number,
                    volume,
                    chapterId,
                    c.optString("group").ifEmpty { null },
                    c.optLong("published", 0L),
                    branch,
                ),
            )
        }
        return cursor
    }

    private fun pages(api: Api, chapterId: String): Cursor {
        val urls = api.pages(chapterId)
        val cursor = MatrixCursor(arrayOf("id", "url", "preview"))
        for (i in 0 until urls.length()) {
            val url = api.absolute(urls.getString(i))
            cursor.addRow(arrayOf<Any?>(stableId("page:$chapterId:$i"), url, null))
        }
        return cursor
    }

    private fun filter(kind: String): Cursor = when (kind) {
        "tags" -> {
            val cursor = MatrixCursor(arrayOf("key", "title"))
            if (prefs.signedIn) {
                runCatching { api().tags() }.getOrNull()?.strings()?.forEach {
                    val key = tagKey(it)
                    cursor.addRow(arrayOf(key, key))
                }
            }
            cursor
        }
        else -> MatrixCursor(arrayOf("name"))
    }

    private fun capabilities() = MatrixCursor(
        arrayOf("sort_orders", "multiple_tags", "tags_exclusion", "search", "search_with_filters"),
    ).apply { addRow(arrayOf<Any>("UPDATED,ALPHABETICAL,NEWEST", 1, 0, 1, 1)) }

    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = throw UnsupportedOperationException()
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = throw UnsupportedOperationException()
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int =
        throw UnsupportedOperationException()

    companion object {
        /** Release, debug, and nightly builds of Kotatsu and Futon. */
        val READER_PACKAGES = listOf("org.koitharu.kotatsu", "io.github.landwarderer.futon")
            .flatMap { listOf(it, "$it.debug", "$it.nightly") }.toSet()

        private val MANGA_COLUMNS = arrayOf(
            "id", "title", "alt_title", "url", "public_url", "rating", "is_nsfw", "cover_url",
            "tags", "state", "author", "large_cover_url", "description",
        )
        private val CHAPTER_COLUMNS = arrayOf(
            "id", "name", "number", "volume", "url", "scanlator", "upload_date", "branch",
        )

        private fun JSONArray?.strings(): List<String> =
            if (this == null) emptyList() else List(length()) { getString(it) }

        /**
         * MangaDex language codes as BCP 47 tags. "es-la" is Latin American
         * Spanish, not Spanish from Laos, and "ja-ro" is romanized Japanese,
         * not Japanese from Romania.
         */
        fun languageTag(code: String): String = when {
            code == "es-la" -> "es-419"
            code.endsWith("-ro") -> code.removeSuffix("-ro") + "-Latn"
            else -> code
        }

        /** Kotatsu packs tags as "key=title:key=title", so those characters can't appear in a tag. */
        private fun tagKey(tag: String) = tag.replace(':', ' ').replace('=', ' ')

        private fun state(status: String) = when (status) {
            "ongoing" -> "ONGOING"
            "completed" -> "FINISHED"
            "hiatus" -> "PAUSED"
            "cancelled" -> "ABANDONED"
            else -> null
        }

        /** 64-bit FNV-1a, so IDs stay the same across sessions and devices. */
        fun stableId(s: String): Long {
            var h = -0x340d631b7bdddcdbL
            for (b in s.toByteArray()) {
                h = h xor (b.toLong() and 0xff)
                h *= 0x100000001b3L
            }
            return h
        }
    }
}
