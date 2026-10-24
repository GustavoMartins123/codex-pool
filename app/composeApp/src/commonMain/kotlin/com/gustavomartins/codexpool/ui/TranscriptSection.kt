package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp

enum class TranscriptTopic { User, Thinking, Tools, Answer, Summary }
data class TranscriptSection(val turnId: String, val topic: TranscriptTopic, val entries: List<TranscriptEntry>)

fun transcriptSections(entries: List<TranscriptEntry>): List<TranscriptSection> {
    val sections = mutableListOf<TranscriptSection>()
    entries.forEach { entry ->
        val topic = when (entry.kind) {
            "user" -> TranscriptTopic.User
            "Thinking" -> TranscriptTopic.Thinking
            "ToolCall", "ToolResult" -> TranscriptTopic.Tools
            "Text" -> TranscriptTopic.Answer
            "summary" -> TranscriptTopic.Summary
            else -> error("Tipo de transcrição inválido: ${entry.kind}")
        }
        val last = sections.lastOrNull()
        if (last?.turnId == entry.turnId && last.topic == topic) {
            sections[sections.lastIndex] = last.copy(entries = last.entries + entry)
        } else sections += TranscriptSection(entry.turnId, topic, listOf(entry))
    }
    return sections
}

@Composable
fun TranscriptSectionCard(section: TranscriptSection) {
    val collapsible = section.topic == TranscriptTopic.Thinking || section.topic == TranscriptTopic.Tools
    var expanded by rememberSaveable { mutableStateOf(false) }
    val title = when (section.topic) {
        TranscriptTopic.User -> "Você"
        TranscriptTopic.Thinking -> "Pensando"
        TranscriptTopic.Tools -> "Ferramentas"
        TranscriptTopic.Answer -> "Resposta do agente"
        TranscriptTopic.Summary -> "Turno concluído"
    }
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = when (section.topic) {
        TranscriptTopic.User -> MaterialTheme.colorScheme.surfaceVariant
        TranscriptTopic.Answer -> MaterialTheme.colorScheme.surface
        else -> MaterialTheme.colorScheme.surfaceContainerLow
    })) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            if (collapsible) {
                TextButton(onClick = { expanded = !expanded }, contentPadding = PaddingValues(0.dp)) {
                    Text("$title · ${if (expanded) "Ocultar" else "Mostrar"}", style = MaterialTheme.typography.labelLarge)
                }
                if (!expanded) Text(if (section.topic == TranscriptTopic.Thinking) "Raciocínio do agente" else "Chamadas e resultados de ferramentas",
                    color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
            } else Text(title, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
            if (!collapsible || expanded) section.entries.forEach { entry ->
                if (section.topic == TranscriptTopic.Tools) Text(if (entry.kind == "ToolCall") "Chamada" else "Resultado",
                    color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.labelSmall)
                SelectionContainer {
                    Text(entry.content, style = if (section.topic == TranscriptTopic.Tools) MaterialTheme.typography.bodySmall.copy(fontFamily = FontFamily.Monospace)
                        else MaterialTheme.typography.bodyLarge)
                }
            }
        }
    }
}
