package org.nextcolor.kotodex

import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder

class ApiException(message: String, val status: Int = 0) : IOException(message)

/** Talks to the Kotodex server's JSON API. All calls block, so keep them off the main thread. */
class Api(private val server: String, private val token: String = "") {

    fun login(username: String, password: String, device: String): String {
        val body = JSONObject()
            .put("username", username)
            .put("password", password)
            .put("device", device)
        val res = request("POST", "/api/v1/login", body.toString())
        return JSONObject(res).getString("token")
    }

    fun logout() {
        request("POST", "/api/v1/logout", "{}")
    }

    fun list(offset: Int, sort: String, query: String?, tags: List<String>, followedOnly: Boolean): JSONArray {
        val params = buildList {
            add("offset" to offset.toString())
            add("sort" to sort)
            if (!query.isNullOrBlank()) add("query" to query)
            tags.forEach { add("tag" to it) }
            if (followedOnly) add("followed" to "1")
        }
        return JSONArray(request("GET", "/api/v1/manga?" + params.joinToString("&") { (k, v) -> "$k=${enc(v)}" }))
    }

    fun manga(id: String): JSONObject = JSONObject(request("GET", "/api/v1/manga/${enc(id)}"))

    fun pages(chapterId: String): JSONArray = JSONArray(request("GET", "/api/v1/chapters/${enc(chapterId)}/pages"))

    fun tags(): JSONArray = JSONArray(request("GET", "/api/v1/tags"))

    /** Turns a server-relative link into a full URL Kotatsu can load. */
    fun absolute(path: String): String = if (path.startsWith("/")) server + path else path

    private fun request(method: String, path: String, body: String? = null): String {
        val conn = URL(server + path).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 15_000
            conn.readTimeout = 30_000
            conn.setRequestProperty("Accept", "application/json")
            if (token.isNotEmpty()) conn.setRequestProperty("Authorization", "Bearer $token")
            if (body != null) {
                conn.doOutput = true
                conn.setRequestProperty("Content-Type", "application/json")
                conn.outputStream.use { it.write(body.toByteArray()) }
            }
            val status = conn.responseCode
            val stream = if (status in 200..299) conn.inputStream else conn.errorStream
            val text = stream?.bufferedReader()?.use { it.readText() }.orEmpty()
            if (status !in 200..299) {
                val msg = runCatching { JSONObject(text).getString("error") }.getOrNull() ?: "HTTP $status"
                throw ApiException(msg, status)
            }
            return text
        } finally {
            conn.disconnect()
        }
    }

    companion object {
        private fun enc(s: String) = URLEncoder.encode(s, "UTF-8")

        /** Accepts "host:port" or a full URL, and drops any trailing slash. */
        fun normalizeServer(input: String): String {
            var s = input.trim().trimEnd('/')
            if (!s.startsWith("http://") && !s.startsWith("https://")) s = "http://$s"
            return s
        }

        /**
         * What to type as the email in Kotatsu's sync sign-in. Kotatsu only
         * takes addresses ending in 2 to 6 letters, so servers reached by IP
         * address or a bare name get a reserved name that can never be real.
         * The server only reads the part before the "@".
         */
        fun syncEmail(server: String, username: String): String {
            val host = runCatching { java.net.URI(server).host }.getOrNull().orEmpty()
            val domain = if (Regex("^[A-Z0-9.-]+\\.[A-Z]{2,6}$", RegexOption.IGNORE_CASE).matches(host)) host else "kotodex.home.arpa"
            return "$username@$domain"
        }
    }
}
