package org.nextcolor.kotodex

import android.content.Intent
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import org.nextcolor.kotodex.ui.KotodexTheme
import org.nextcolor.kotodex.ui.MainScreen
import org.nextcolor.kotodex.ui.ScreenState
import java.io.IOException
import kotlin.concurrent.thread

class MainActivity : ComponentActivity() {

    private lateinit var prefs: Prefs
    private var state by mutableStateOf(ScreenState(false, "", "", false, false))

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        prefs = Prefs(this)
        setContent {
            KotodexTheme {
                MainScreen(
                    state = state,
                    onSignIn = ::signIn,
                    onSignOut = ::signOut,
                    onFollowedOnly = {
                        prefs.followedOnly = it
                        refresh()
                    },
                    onOpenKotatsu = { kotatsuIntent()?.let(::startActivity) },
                    onAllowApp = {
                        prefs.allow(it)
                        refresh()
                    },
                    onRemoveApp = {
                        prefs.disallow(it)
                        refresh()
                    },
                )
            }
        }
    }

    override fun onResume() {
        super.onResume()
        refresh()
    }

    private fun refresh(error: String? = null, busy: Boolean = false) {
        state = ScreenState(
            signedIn = prefs.signedIn,
            server = prefs.server,
            username = prefs.username,
            followedOnly = prefs.followedOnly,
            kotatsuInstalled = kotatsuIntent() != null,
            requestedApps = prefs.requestedApps.associateWith(::appName),
            allowedApps = prefs.allowedApps.associateWith(::appName),
            busy = busy,
            error = error,
        )
    }

    /** The app's name, or its package name when Android doesn't let Kotodex see it. */
    private fun appName(pkg: String): String = runCatching {
        packageManager.getApplicationLabel(packageManager.getApplicationInfo(pkg, 0)).toString()
    }.getOrDefault(pkg)

    private fun kotatsuIntent(): Intent? = packageManager.getLaunchIntentForPackage("org.koitharu.kotatsu")

    private fun signIn(serverInput: String, username: String, password: String) {
        val server = Api.normalizeServer(serverInput)
        state = state.copy(busy = true, error = null)
        thread {
            val error = try {
                val token = Api(server).login(username.trim(), password, "Kotatsu on ${Build.MANUFACTURER} ${Build.MODEL}")
                prefs.server = server
                prefs.username = username.trim()
                prefs.token = token
                null
            } catch (e: ApiException) {
                e.message
            } catch (e: IOException) {
                "Can't reach the server: ${e.message}"
            } catch (e: IllegalArgumentException) {
                "That server address isn't valid"
            }
            runOnUiThread { refresh(error) }
        }
    }

    private fun signOut() {
        val api = Api(prefs.server, prefs.token)
        prefs.token = ""
        refresh()
        // Ends the session on the server too. Signed out locally either way.
        thread { runCatching { api.logout() } }
    }
}
