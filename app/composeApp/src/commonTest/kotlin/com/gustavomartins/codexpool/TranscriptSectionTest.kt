package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.ui.*
import kotlin.test.*

class TranscriptSectionTest {
    @Test fun transcriptKeepsAnswerSeparateAndToolResultsTogetherWithoutMixingTurns() {
        val entries = listOf(TranscriptEntry("one", "Thinking", "reason"), TranscriptEntry("one", "ToolCall", "command"),
            TranscriptEntry("one", "ToolResult", "output"), TranscriptEntry("one", "Text", "answer"), TranscriptEntry("two", "Text", "next answer"))
        val sections = transcriptSections(entries)
        assertEquals(listOf(TranscriptTopic.Thinking, TranscriptTopic.Tools, TranscriptTopic.Answer, TranscriptTopic.Answer), sections.map { it.topic })
        assertEquals(listOf("command", "output"), sections[1].entries.map { it.content })
        assertEquals("answer", sections[2].entries.single().content)
        assertEquals(entries, sections.flatMap { it.entries })
    }
}
