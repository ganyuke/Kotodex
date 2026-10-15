package org.nextcolor.kotodex

import android.database.Cursor
import android.net.Uri
import com.sun.net.httpserver.HttpServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import java.net.InetSocketAddress

/**
 * Queries the provider with the same URIs and column names Kotatsu's
 * ExternalPluginContentSource uses, against a stub of the server API.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35])
class ProviderTest {
    private lateinit var server: HttpServer
    private lateinit var provider: MangaProvider
    private val requests = mutableListOf<String>()
    private val auth = "content://org.nextcolor.kotodex.manga"

    private val manga = """{"id":"m1","title":"Akanabe","altTitles":["茜部先生"],"description":"Desc",
        "authors":["Naoya Tajimi","Someone"],"status":"ongoing","contentRating":"erotica","tags":["Comedy","Sci:Fi"],
        "coverUrl":"/img/1/s/m1/thumb.jpg","largeCoverUrl":"/img/1/s/m1/cover.jpg","publicUrl":"https://mangadex.org/title/m1"}"""

    @Before fun setUp() {
        server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/") { ex ->
            requests += ex.requestURI.toString()
            val (status, body) = when {
                ex.requestHeaders.getFirst("Authorization") != "Bearer good" -> 401 to """{"error":"Signed out"}"""
                ex.requestURI.path == "/api/v1/manga" -> 200 to "[$manga]"
                ex.requestURI.path == "/api/v1/manga/m1" -> 200 to manga.dropLast(1) + ""","chapterList":[
                    {"id":"c1","title":"","number":1,"volume":1,"lang":"en","group":"G","published":1000},
                    {"id":"c2","title":"Two","number":2,"volume":1,"lang":"en","group":"G","published":2000},
                    {"id":"c3","title":"Two again","number":2,"volume":1,"lang":"en","group":"H","published":3000},
                    {"id":"c4","title":"","number":1,"volume":0,"lang":"es","group":"","published":4000}]}"""
                ex.requestURI.path == "/api/v1/chapters/c2/pages" -> 200 to """["/img/1/s/m1/c2/001.jpg","/img/1/s/m1/c2/002.jpg"]"""
                ex.requestURI.path == "/api/v1/tags" -> 200 to """["Comedy","Sci:Fi"]"""
                else -> 404 to """{"error":"Not found"}"""
            }
            val bytes = body.toByteArray()
            ex.sendResponseHeaders(status, bytes.size.toLong())
            ex.responseBody.use { it.write(bytes) }
        }
        server.start()
        val prefs = Prefs(RuntimeEnvironment.getApplication())
        prefs.server = "http://127.0.0.1:${server.address.port}"
        prefs.token = "good"
        provider = Robolectric.buildContentProvider(MangaProvider::class.java).create().get()
        callAs("org.koitharu.kotatsu")
    }

    private fun callAs(pkg: String) = shadowOf(provider).setCallingPackage(pkg)

    @After fun tearDown() = server.stop(0)

    private fun query(uri: String, sort: String? = null): Cursor =
        provider.query(Uri.parse(uri), null, null, null, sort)

    private fun Cursor.str(col: String): String? = getColumnIndex(col).let { if (it < 0 || isNull(it)) null else getString(it) }

    @Test fun list() {
        val c = query("$auth/manga?offset=50&query=aka%20be&tags_include=Comedy%3DComedy", "ALPHABETICAL")
        assertEquals(1, c.count)
        c.moveToFirst()
        assertEquals("Akanabe", c.str("title"))
        assertEquals("m1", c.str("url"))
        assertEquals(-1f, c.getFloat(c.getColumnIndexOrThrow("rating")))
        assertEquals(1, c.getInt(c.getColumnIndexOrThrow("is_nsfw")))
        assertEquals("ONGOING", c.str("state"))
        assertEquals("Naoya Tajimi,Someone", c.str("author"))
        assertEquals("Comedy=Comedy:Sci Fi=Sci Fi", c.str("tags"))
        assertTrue(c.str("cover_url")!!.startsWith("http://127.0.0.1:"))
        assertTrue(c.str("cover_url")!!.endsWith("/img/1/s/m1/thumb.jpg"))
        assertEquals("/api/v1/manga?offset=50&sort=title&query=aka+be&tag=Comedy", requests.last())
    }

    @Test fun detailsAndChapters() {
        val d = query("$auth/manga/m1")
        d.moveToFirst()
        assertEquals("Desc", d.str("description"))

        val c = query("$auth/manga/chapters/m1")
        assertEquals(4, c.count)
        val branches = mutableListOf<String?>()
        val names = mutableListOf<String?>()
        while (c.moveToNext()) {
            branches += c.str("branch")
            names += c.str("name")
        }
        // The second upload of chapter 2 goes to its own branch.
        assertEquals(listOf("English", "English", "English (1)", "Español"), branches)
        assertNull(names[0])
        c.moveToFirst()
        assertEquals("c1", c.str("url"))
        assertEquals(1000L, c.getLong(c.getColumnIndexOrThrow("upload_date")))
    }

    @Test fun pages() {
        val c = query("$auth/chapters/c2")
        assertEquals(2, c.count)
        c.moveToLast()
        assertTrue(c.str("url")!!.endsWith("/img/1/s/m1/c2/002.jpg"))
        val p = query("$auth/manga/pages/0?url=" + Uri.encode(c.str("url")))
        p.moveToFirst()
        assertEquals(c.str("url"), p.str("value"))
    }

    @Test fun filtersAndCapabilities() {
        val t = query("$auth/filter/tags")
        assertEquals(2, t.count)
        assertEquals(0, query("$auth/filter/states").count)
        val cap = query("$auth/capabilities")
        cap.moveToFirst()
        assertEquals("UPDATED,ALPHABETICAL,NEWEST", cap.str("sort_orders"))
        assertEquals(1, cap.getInt(cap.getColumnIndexOrThrow("search")))
    }

    @Test fun signedOutOnServer() {
        Prefs(RuntimeEnvironment.getApplication()).token = "revoked"
        try {
            query("$auth/manga")
            fail("expected an error")
        } catch (e: IllegalStateException) {
            assertTrue(e.message!!.contains("sign in again"))
        }
    }

    @Test fun otherAppsNeedApproval() {
        val prefs = Prefs(RuntimeEnvironment.getApplication())
        callAs("com.example.snoop")
        try {
            query("$auth/manga")
            fail("an unknown app should be refused")
        } catch (e: SecurityException) {
            assertTrue(e.message!!.contains("com.example.snoop"))
        }
        assertEquals(setOf("com.example.snoop"), prefs.requestedApps)
        assertTrue(requests.isEmpty())

        prefs.allowedApps = setOf("com.example.snoop")
        assertEquals(1, query("$auth/manga").count)
    }

    @Test fun stableIds() {
        assertEquals(MangaProvider.stableId("manga:m1"), MangaProvider.stableId("manga:m1"))
        assertTrue(MangaProvider.stableId("manga:m1") != MangaProvider.stableId("manga:m2"))
    }
}
