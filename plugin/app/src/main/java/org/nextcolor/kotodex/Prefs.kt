package org.nextcolor.kotodex

import android.content.Context
import android.content.SharedPreferences

/** Server address, account and session token. Kept in private app storage. */
class Prefs(context: Context) {
    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences("kotodex", Context.MODE_PRIVATE)

    var server: String
        get() = prefs.getString("server", "").orEmpty()
        set(value) = prefs.edit().putString("server", value).apply()

    var username: String
        get() = prefs.getString("username", "").orEmpty()
        set(value) = prefs.edit().putString("username", value).apply()

    var token: String
        get() = prefs.getString("token", "").orEmpty()
        set(value) = prefs.edit().putString("token", value).apply()

    /** Show only manga the signed-in user follows, instead of the whole server library. */
    var followedOnly: Boolean
        get() = prefs.getBoolean("followed_only", false)
        set(value) = prefs.edit().putBoolean("followed_only", value).apply()

    /** Apps besides Kotatsu that the user let read the library. */
    var allowedApps: Set<String>
        get() = prefs.getStringSet("allowed_apps", null).orEmpty().toSet()
        set(value) = prefs.edit().putStringSet("allowed_apps", value).apply()

    /** Apps that tried to read the library and were refused, waiting for the user to decide. */
    var requestedApps: Set<String>
        get() = prefs.getStringSet("requested_apps", null).orEmpty().toSet()
        set(value) = prefs.edit().putStringSet("requested_apps", value).apply()

    // Kotatsu's calls arrive on several threads while the app may change the
    // lists too, so each change reads and writes under one lock.
    fun request(pkg: String) = synchronized(lock) { requestedApps = requestedApps + pkg }

    fun allow(pkg: String) = synchronized(lock) {
        allowedApps = allowedApps + pkg
        requestedApps = requestedApps - pkg
    }

    fun disallow(pkg: String) = synchronized(lock) { allowedApps = allowedApps - pkg }

    val signedIn: Boolean get() = server.isNotEmpty() && token.isNotEmpty()

    private companion object {
        val lock = Any()
    }
}
