package com.gustavomartins.codexpool

import androidx.compose.foundation.layout.*
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
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
class ModelSettingsDeviceTest {
    @get:Rule val compose = createComposeRule()
    private val catalog = listOf(
        CatalogModel("gpt-test", "GPT test", "openai", true, reasoningEfforts = listOf("low", "high", "max")),
        CatalogModel("gemini-test", "Gemini test", "gemini", true, protocols = listOf("responses", "anthropic"), reasoningEfforts = listOf("minimal", "low", "high")),
        CatalogModel("claude-test", "Claude test", "anthropic", true, reasoningEfforts = listOf("low", "medium", "high")),
    )

    @Test fun changingTheModelReplacesTheAvailableReasoningOptions() {
        var selected by mutableStateOf(TurnSettings("gpt-test", "max"))
        compose.setContent {
            MaterialTheme { Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                ModelSettingsFields(Agent.Codex, catalog, selected, true) { selected = it }
            } }
        }
        compose.onNodeWithTag("effort-selector").assertTextContains("M\u00e1ximo")
        compose.onNodeWithTag("model-selector").performClick()
        compose.onNodeWithText("Claude test").assertDoesNotExist()
        compose.onNodeWithText("Gemini test").performClick()
        compose.runOnIdle { assertEquals(TurnSettings("gemini-test", ""), selected) }
        compose.onNodeWithTag("effort-selector").performClick()
        compose.onNodeWithText("M\u00e1ximo").assertDoesNotExist()
        compose.onNodeWithText("M\u00ednimo").performClick()
        compose.runOnIdle { assertEquals("minimal", selected.reasoningEffort) }
    }

    @Test fun claudeDoesNotOfferReasoningValuesItsControlProtocolCannotSend() {
        var selected by mutableStateOf(TurnSettings("gemini-test", "low"))
        compose.setContent {
            MaterialTheme { Column(Modifier.padding(20.dp)) { ModelSettingsFields(Agent.Claude, catalog, selected, true) { selected = it } } }
        }
        compose.onNodeWithTag("effort-selector").performClick()
        compose.onNodeWithText("M\u00ednimo").assertDoesNotExist()
        compose.onNodeWithText("Alto").performClick()
        compose.runOnIdle { assertEquals("high", selected.reasoningEffort) }
    }
}
