package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.ui.*
import kotlin.test.*

class TranscriptSectionTest {
    @Test fun transcriptKeepsAnswerSeparateAndToolResultsTogetherWithoutMixingTurns() {
        val entries = listOf(TranscriptEntry("one", "Thinking", "reason", 1L), TranscriptEntry("one", "ToolCall", "command", 2L),
            TranscriptEntry("one", "ToolResult", "output", 3L), TranscriptEntry("one", "Text", "answer", 4L), TranscriptEntry("two", "Text", "next answer", 5L))
        val sections = transcriptSections(entries)
        assertEquals(listOf(TranscriptTopic.Thinking, TranscriptTopic.Tools, TranscriptTopic.Answer, TranscriptTopic.Answer), sections.map { it.topic })
        assertEquals(listOf("command", "output"), sections[1].entries.map { it.content })
        assertEquals("answer", sections[2].entries.single().content)
        assertEquals(entries, sections.flatMap { it.entries })
    }

    @Test fun completedTurnIsASeparateLowPriorityMetadataSection() {
        val sections = transcriptSections(listOf(
            TranscriptEntry("one", "Text", "answer", 6L),
            TranscriptEntry("one", "summary", "12 tokens · 340 ms", 7L),
        ))
        assertEquals(TranscriptTopic.Answer, sections[0].topic)
        assertEquals(TranscriptTopic.Summary, sections[1].topic)
        assertEquals("12 tokens · 340 ms", sections[1].entries.single().content)
    }
}
