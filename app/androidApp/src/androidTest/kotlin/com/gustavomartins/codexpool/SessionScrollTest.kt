package com.gustavomartins.codexpool

import androidx.compose.foundation.layout.height
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.compose.ui.unit.dp
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class SessionScrollTest {
    @get:Rule val compose = createComposeRule()

    @Test fun growingStreamFollowsTheBottomUntilTheUserScrollsUp() {
        var state by mutableStateOf(SessionScreenState(
            entries = listOf(TranscriptEntry("t", "Text", (1..40).joinToString("\n") { "Stream line $it" }, 1L)),
        ))
        compose.setContent {
            MaterialTheme { SessionEventList(state, Modifier.height(220.dp), onReconnect = {}, onDecision = { _, _ -> }, onLoadOlder = {}, onResume = {}) }
        }
        compose.waitForIdle()
        compose.onNodeWithTag("session-latest").assertIsDisplayed()
        compose.runOnIdle {
            state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t", DeltaKind.Text,
                (41..60).joinToString("\n", prefix = "\n") { "Stream line $it" })), 1L)
            assertEquals(1, state.entries.size)
        }
        compose.waitForIdle()
        compose.onNodeWithTag("session-latest").assertIsDisplayed()

        compose.onNodeWithTag("session-events").performTouchInput { swipeDown() }
        compose.waitForIdle()
        compose.onNodeWithTag("session-latest").assertIsNotDisplayed()
        val position = scrollPosition()
        compose.runOnIdle {
            state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t", DeltaKind.Text,
                (61..80).joinToString("\n", prefix = "\n") { "Stream line $it" })), 2L)
        }
        compose.waitForIdle()
        compose.onNodeWithTag("session-latest").assertIsNotDisplayed()
        assertEquals(position, scrollPosition(), 0.01f)
    }

    private fun scrollPosition() = compose.onNodeWithTag("session-events").fetchSemanticsNode()
        .config[SemanticsProperties.VerticalScrollAxisRange].value()
}
