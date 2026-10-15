package org.nextcolor.kotodex.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.OpenInNew
import androidx.compose.material.icons.rounded.Apps
import androidx.compose.material.icons.rounded.Dns
import androidx.compose.material.icons.rounded.FilterList
import androidx.compose.material.icons.rounded.Person
import androidx.compose.material.icons.rounded.Visibility
import androidx.compose.material.icons.rounded.VisibilityOff
import androidx.compose.material3.Button
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MediumFlexibleTopAppBar
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedListItem
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp

data class ScreenState(
    val signedIn: Boolean,
    val server: String,
    val username: String,
    val followedOnly: Boolean,
    val kotatsuInstalled: Boolean,
    /** Package name to app name, for apps that asked for access and are waiting. */
    val requestedApps: Map<String, String> = emptyMap(),
    /** Package name to app name, for apps the user allowed besides Kotatsu. */
    val allowedApps: Map<String, String> = emptyMap(),
    val busy: Boolean = false,
    val error: String? = null,
)

@Composable
fun MainScreen(
    state: ScreenState,
    onSignIn: (server: String, username: String, password: String) -> Unit,
    onSignOut: () -> Unit,
    onFollowedOnly: (Boolean) -> Unit,
    onOpenKotatsu: () -> Unit,
    onAllowApp: (String) -> Unit = {},
    onRemoveApp: (String) -> Unit = {},
) {
    val behavior = TopAppBarDefaults.exitUntilCollapsedScrollBehavior()
    Scaffold(
        modifier = Modifier.nestedScroll(behavior.nestedScrollConnection),
        containerColor = MaterialTheme.colorScheme.surfaceContainer,
        topBar = {
            MediumFlexibleTopAppBar(
                title = { Text("Kotodex") },
                scrollBehavior = behavior,
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.surfaceContainer,
                    scrolledContainerColor = MaterialTheme.colorScheme.surfaceContainer,
                ),
            )
        },
    ) { padding ->
        LazyColumn(
            Modifier.fillMaxSize().imePadding(),
            contentPadding = PaddingValues(
                start = 16.dp,
                end = 16.dp,
                top = padding.calculateTopPadding(),
                bottom = padding.calculateBottomPadding() + 24.dp,
            ),
            verticalArrangement = Arrangement.spacedBy(ListItemDefaults.SegmentedGap),
        ) {
            if (!state.signedIn) {
                item { SectionHeader("Sign in") }
                item { SignInForm(state, onSignIn) }
            } else {
                item { SectionHeader("Server") }
                item {
                    Row(Icons.Rounded.Dns, "Server address", state.server, 0, 2)
                }
                item {
                    Row(Icons.Rounded.Person, "Account", state.username, 1, 2) {
                        OutlinedButton(onSignOut, enabled = !state.busy) { Text("Sign out") }
                    }
                }

                item { SectionHeader("Library") }
                item {
                    SegmentedListItem(
                        onClick = { onFollowedOnly(!state.followedOnly) },
                        shapes = ListItemDefaults.segmentedShapes(0, 1),
                        colors = ListItemDefaults.segmentedColors(containerColor = MaterialTheme.colorScheme.surfaceBright),
                        leadingContent = { Icon(Icons.Rounded.FilterList, contentDescription = null) },
                        trailingContent = { Switch(checked = state.followedOnly, onCheckedChange = onFollowedOnly) },
                    ) { Text("Only manga I follow") }
                }

                item { SectionHeader("Kotatsu") }
                item {
                    SegmentedListItem(
                        onClick = onOpenKotatsu,
                        enabled = state.kotatsuInstalled,
                        shapes = ListItemDefaults.segmentedShapes(0, 1),
                        colors = ListItemDefaults.segmentedColors(containerColor = MaterialTheme.colorScheme.surfaceBright),
                        leadingContent = { Icon(Icons.AutoMirrored.Rounded.OpenInNew, contentDescription = null) },
                        supportingContent = {
                            Text(
                                if (state.kotatsuInstalled) "Kotodex is pinned at the top of Explore"
                                else "Not installed",
                            )
                        },
                    ) { Text("Open Kotatsu") }
                }

                val apps = state.requestedApps.map { it to true } + state.allowedApps.map { it to false }
                if (apps.isNotEmpty()) {
                    item { SectionHeader("Other apps") }
                    apps.forEachIndexed { i, (app, waiting) ->
                        item(key = app.key) {
                            Row(Icons.Rounded.Apps, app.value, if (waiting) "Asked to read your library" else "Allowed", i, apps.size) {
                                if (waiting) {
                                    Button({ onAllowApp(app.key) }) { Text("Allow") }
                                } else {
                                    OutlinedButton({ onRemoveApp(app.key) }) { Text("Remove") }
                                }
                            }
                        }
                    }
                }
            }
            state.error?.let { msg ->
                item {
                    Text(
                        msg,
                        Modifier.padding(start = 16.dp, top = 12.dp, end = 16.dp),
                        color = MaterialTheme.colorScheme.error,
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
        }
    }
}

@Composable
private fun SignInForm(state: ScreenState, onSignIn: (String, String, String) -> Unit) {
    var server by rememberSaveable { mutableStateOf(state.server) }
    var username by rememberSaveable { mutableStateOf(state.username) }
    var password by rememberSaveable { mutableStateOf("") }
    var showPassword by rememberSaveable { mutableStateOf(false) }
    val canSubmit = server.isNotBlank() && username.isNotBlank() && password.isNotEmpty() && !state.busy
    val submit = { if (canSubmit) onSignIn(server, username, password) }

    Surface(
        color = MaterialTheme.colorScheme.surfaceBright,
        shape = MaterialTheme.shapes.large,
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            OutlinedTextField(
                value = server,
                onValueChange = { server = it },
                label = { Text("Server address") },
                placeholder = { Text("https://manga.example.com") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = username,
                onValueChange = { username = it },
                label = { Text("Username") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = password,
                onValueChange = { password = it },
                label = { Text("Password") },
                singleLine = true,
                visualTransformation = if (showPassword) VisualTransformation.None else PasswordVisualTransformation(),
                trailingIcon = {
                    IconButton({ showPassword = !showPassword }) {
                        Icon(
                            if (showPassword) Icons.Rounded.VisibilityOff else Icons.Rounded.Visibility,
                            contentDescription = if (showPassword) "Hide password" else "Show password",
                        )
                    }
                },
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Done),
                keyboardActions = KeyboardActions(onDone = { submit() }),
                modifier = Modifier.fillMaxWidth(),
            )
            Button(onClick = submit, enabled = canSubmit, modifier = Modifier.align(Alignment.End)) {
                if (state.busy) LoadingIndicator(Modifier.padding(end = 8.dp).size(24.dp))
                Text("Sign in")
            }
        }
    }
}

@Composable
private fun SectionHeader(text: String) {
    Text(
        text,
        Modifier.padding(start = 16.dp, top = 20.dp, bottom = 6.dp),
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.primary,
    )
}

@Composable
private fun Row(
    icon: ImageVector,
    headline: String,
    supporting: String,
    index: Int,
    count: Int,
    trailing: (@Composable () -> Unit)? = null,
) {
    SegmentedListItem(
        onClick = {},
        shapes = ListItemDefaults.segmentedShapes(index, count),
        colors = ListItemDefaults.segmentedColors(containerColor = MaterialTheme.colorScheme.surfaceBright),
        leadingContent = { Icon(icon, contentDescription = null) },
        supportingContent = { Text(supporting) },
        trailingContent = trailing,
    ) { Text(headline) }
}
