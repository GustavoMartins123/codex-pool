package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.interaction.collectIsDraggedAsState
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.gustavomartins.codexpool.data.*
import kotlinx.coroutines.flow.first

@Composable
fun SessionEventList(
    state: SessionScreenState,
    modifier: Modifier = Modifier,
    onReconnect: () -> Unit,
    onDecision: (String, Decision) -> Unit,
    onLoadOlder: () -> Unit,
    onResume: () -> Unit,
) {
    val scroll = rememberLazyListState()
    val sections = remember(state.entries) { transcriptSections(state.entries) }
    var followLatest by remember { mutableStateOf(true) }
    var userScrolled by remember { mutableStateOf(false) }
    val dragging by scroll.interactionSource.collectIsDraggedAsState()
    LaunchedEffect(dragging) {
        if (dragging) {
            userScrolled = true
            followLatest = false
        } else if (userScrolled) {
            snapshotFlow { scroll.isScrollInProgress }.first { !it }
            followLatest = !scroll.canScrollForward
            userScrolled = false
        }
    }
    val viewportHeight = scroll.layoutInfo.viewportSize.height
    LaunchedEffect(state.entries, state.approvals, followLatest, viewportHeight) {
        if (followLatest) {
            // Apply at the next measurement, including text that grows inside
            // an existing card. The final anchor reaches the end of tall cards.
            scroll.requestScrollToItem(1 + state.approvals.size + sections.size)
        }
    }
    LazyColumn(modifier.fillMaxSize().testTag("session-events"), state = scroll, contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        item {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                state.session?.let { session ->
                    if (state.conversation?.storage == ConversationStorage.Archived) Text("Conversa arquivada", style = MaterialTheme.typography.titleLarge)
                    Text(session.state.label(), style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.primary)
                    SelectionContainer { Text(session.id, style = MaterialTheme.typography.labelSmall) }
                    Text(session.runnerId?.let { "Runner: $it" } ?: "Aguardando runner do seu principal", color = MaterialTheme.colorScheme.onSurfaceVariant)
                    session.closedReason?.let { Text("Encerrada: $it") }
                    if (session.state == SessionState.Closed) {
                        if (state.conversation?.deleting == true) Text("Exclusão em andamento…")
                        else if (state.conversation?.resumeAvailable == true) {
                            Button(onClick = onResume, enabled = !state.busy) { Text("Retomar conversa") }
                        } else Text("O histórico está preservado. A retomada desta execução não está disponível.",
                            color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
                    }
                }
                Text(when (state.connection) {
                    StreamConnection.Live -> "● Ao vivo"
                    StreamConnection.Connecting -> "Conectando ao canal de eventos…"
                    StreamConnection.Disconnected -> "Canal de eventos desconectado"
                }, style = MaterialTheme.typography.labelSmall)
                state.runnerStatus?.let { status -> Text(when (status) {
                    RunnerStatus.Online -> "Runner online"
                    RunnerStatus.Offline -> "Runner offline · aguardando reconexão da máquina"
                    RunnerStatus.Reconnected -> "Runner reconectado"
                }) }
                ErrorMessage(state.error)
                if (state.connection == StreamConnection.Disconnected && state.session?.state != SessionState.Closed) {
                    OutlinedButton(onClick = onReconnect) { Text("Reconectar") }
                }
                if (state.connection == StreamConnection.Connecting || state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                ErrorMessage(state.historyError)
                if (state.hasOlder) OutlinedButton(onClick = onLoadOlder, enabled = !state.loadingOlder) {
                    Text(if (state.loadingOlder) "Carregando…" else "Carregar mensagens anteriores")
                }
            }
        }
        items(state.approvals, key = { "approval/${it.approvalId}" }) { approval ->
            OutlinedCard(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    Text("Aprovação necessária", color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.titleLarge)
                    SelectionContainer { Text(approval.command, style = MaterialTheme.typography.labelSmall) }
                    Text(approval.context)
                    Text("Sem decisão, o broker nega após 10 minutos.", style = MaterialTheme.typography.bodySmall)
                    Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        OutlinedButton(onClick = { onDecision(approval.approvalId, Decision.Deny) }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Negar") }
                        Button(onClick = { onDecision(approval.approvalId, Decision.Approve) }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Aprovar") }
                    }
                }
            }
        }
        items(sections, key = { "transcript/${it.id}" }) { section ->
            TranscriptSectionCard(section)
        }

        item(key = "session-latest") { Spacer(Modifier.fillMaxWidth().height(1.dp).testTag("session-latest")) }
    }
}
