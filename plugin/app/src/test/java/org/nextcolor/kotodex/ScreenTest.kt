package org.nextcolor.kotodex

import androidx.activity.ComponentActivity
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onRoot
import com.github.takahirom.roborazzi.captureRoboImage
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.nextcolor.kotodex.ui.KotodexTheme
import org.nextcolor.kotodex.ui.MainScreen
import org.nextcolor.kotodex.ui.ScreenState
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/** Renders the screens to app/build/screenshots so they can be looked at without a phone. */
@RunWith(RobolectricTestRunner::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE)
@Config(sdk = [35], qualifiers = "w412dp-h915dp-xhdpi")
class ScreenTest {
    @get:Rule val rule = createAndroidComposeRule<ComponentActivity>()

    private fun shot(name: String, state: ScreenState, dark: Boolean = false) {
        rule.setContent {
            KotodexTheme(dark = dark) { MainScreen(state, { _, _, _ -> }, {}, {}, {}) }
        }
        rule.onRoot().captureRoboImage("build/screenshots/$name.png")
    }

    @Test fun signedOut() = shot("1-signed-out", ScreenState(false, "", "", false, true))

    @Test fun signInError() = shot(
        "2-sign-in-error",
        ScreenState(false, "https://manga.example.com", "mio", false, true, error = "Wrong username or password"),
    )

    @Test fun signedIn() = shot("3-signed-in", ScreenState(true, "https://manga.example.com", "mio", true, true))

    @Test fun signedInDark() = shot("4-signed-in-dark", ScreenState(true, "http://192.168.1.20:8080", "mio", false, false), dark = true)

    @Test fun otherApps() = shot(
        "5-other-apps",
        ScreenState(
            true, "https://manga.example.com", "mio", false, true,
            requestedApps = mapOf("app.example.reader" to "Some Reader"),
            allowedApps = mapOf("org.example.kotatsufork" to "Kotatsu Fork"),
        ),
    )

    @Test fun normalizeServer() {
        assertEquals("http://192.168.1.20:8080", Api.normalizeServer(" 192.168.1.20:8080/ "))
        assertEquals("https://manga.example.com", Api.normalizeServer("https://manga.example.com/"))
    }

    @Test fun syncEmail() {
        assertEquals("mio@manga.example.com", Api.syncEmail("https://manga.example.com", "mio"))
        assertEquals("mio@kotodex.home.arpa", Api.syncEmail("http://192.168.1.20:8080", "mio"))
        assertEquals("mio@kotodex.home.arpa", Api.syncEmail("http://nas:8080", "mio"))
        assertEquals("mio@kotodex.home.arpa", Api.syncEmail("https://manga.photography", "mio"))
    }
}
