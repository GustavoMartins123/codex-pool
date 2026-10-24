package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.ui.TranscriptEntry
import com.gustavomartins.codexpool.ui.transcriptSections
import com.gustavomartins.codexpool.ui.TranscriptSectionCard
import androidx.compose.foundation.layout.Column
import androidx.compose.material3.MaterialTheme
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class TranscriptPresentationTest {
    @get:Rule val compose = createComposeRule()
    @Test fun thinkingIsVisibleAndCanBeCollapsedWhileToolsStartCollapsed() {
        val sections = transcriptSections(listOf(TranscriptEntry("t", "Thinking", "private-ui-reason", 1L),
            TranscriptEntry("t", "ToolCall", "synthetic command", 2L), TranscriptEntry("t", "ToolResult", "synthetic output", 3L),
            TranscriptEntry("t", "Text", "Resposta legível", 4L)))
        compose.setContent { MaterialTheme { Column { sections.forEach { TranscriptSectionCard(it) } } } }
        compose.onNodeWithText("Resposta legível").assertIsDisplayed()
        compose.onNodeWithText("private-ui-reason").assertIsDisplayed()
        compose.onNodeWithText("Pensando · Ocultar").performClick()
        compose.onNodeWithText("private-ui-reason").assertDoesNotExist()
        compose.onNodeWithText("synthetic command").assertDoesNotExist()
        compose.onNodeWithText("Ferramentas · Mostrar · 2").performClick()
        compose.onNodeWithText("synthetic command").assertIsDisplayed()
        compose.onNodeWithText("synthetic output").assertIsDisplayed()
        compose.onNodeWithText("Ferramentas · Ocultar · 2").performClick()
        compose.onNodeWithText("Pensando · Mostrar").performClick()
        compose.onNodeWithText("private-ui-reason").assertIsDisplayed()
        compose.onNodeWithText("Resposta legível").assertIsDisplayed()
    }

}
