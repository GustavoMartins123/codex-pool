@file:OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)

package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleStartEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.gustavomartins.codexpool.data.*

@Composable
fun SessionScreen(vm: SessionViewModel, onBack: () -> Unit) {
    val state by vm.state.collectAsStateWithLifecycle()
    var input by rememberSaveable { mutableStateOf("") }
    var closeDialog by remember { mutableStateOf(false) }
    val scroll = rememberLazyListState()
    LifecycleStartEffect(vm) {
        vm.connect()
        onStopOrDispose { vm.disconnect() }
    }
    LaunchedEffect(state.entries.size) {
        if (scroll.layoutInfo.totalItemsCount > 0 && !scroll.canScrollForward) scroll.animateScrollToItem(scroll.layoutInfo.totalItemsCount - 1)
    }
    Scaffold(topBar = {
        TopAppBar(title = { Text(state.session?.let { "${it.agent.name} · ${it.model}" } ?: "Sessão") },
            navigationIcon = { TextButton(onClick = onBack) { Text("Voltar") } },
            actions = {
                if (state.session != null && state.session?.state !in listOf(SessionState.Closed, SessionState.Closing)) {
                    TextButton(onClick = { closeDialog = true }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Encerrar", color = MaterialTheme.colorScheme.error) }
                }
            })
    }, bottomBar = {
        if (state.session?.state != SessionState.Closed) {
            Surface(tonalElevation = 4.dp) {
                Column(Modifier.fillMaxWidth().navigationBarsPadding().imePadding().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedTextField(input, { input = it }, Modifier.fillMaxWidth().heightIn(max = 180.dp), label = { Text("Mensagem para o agente") }, enabled = state.canSend)
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        OutlinedButton(onClick = vm::cancel, enabled = state.canCancel) { Text("Cancelar turno") }
                        Button(onClick = { vm.send(input) { input = "" } }, enabled = state.canSend && input.isNotBlank()) { Text("Enviar") }
                    }
                }
            }
        }
    }) { padding ->
        LazyColumn(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding), state = scroll, contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            item {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    state.session?.let { session ->
                        Text(session.state.label(), style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.primary)
                        SelectionContainer { Text(session.id, style = MaterialTheme.typography.labelSmall) }
                        Text(session.runnerId?.let { "Runner: $it" } ?: "Aguardando runner do seu principal", color = MaterialTheme.colorScheme.onSurfaceVariant)
                        session.closedReason?.let { Text("Encerrada: $it") }
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
                        OutlinedButton(onClick = vm::connect) { Text("Reconectar") }
                    }
                    if (state.connection == StreamConnection.Connecting || state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                    Text("Eventos recebidos nesta tela. O agente executa na sua máquina.", color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
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
                            OutlinedButton(onClick = { vm.decide(approval.approvalId, Decision.Deny) }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Negar") }
                            Button(onClick = { vm.decide(approval.approvalId, Decision.Approve) }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Aprovar") }
                        }
                    }
                }
            }
            items(state.entries) { entry ->
                Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = if (entry.kind == "user") MaterialTheme.colorScheme.surfaceVariant else MaterialTheme.colorScheme.surface)) {
                    Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                        Text(when (entry.kind) {
                            "user" -> "VOCÊ"
                            "Text" -> "AGENTE"
                            "Thinking" -> "RACIOCÍNIO"
                            "ToolCall" -> "FERRAMENTA"
                            "ToolResult" -> "RESULTADO"
                            "summary" -> "TURNO CONCLUÍDO"
                            else -> error("Tipo de transcrição inválido: ${entry.kind}")
                        }, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.primary)
                        SelectionContainer { Text(entry.content) }
                    }
                }
            }
        }
    }
    if (closeDialog) AlertDialog(onDismissRequest = { closeDialog = false }, title = { Text("Encerrar sessão?") },
        text = { Text("O runner encerrará o processo do agente e limpará o workspace efêmero. Workspaces por caminho ou repositório serão preservados.") },
        confirmButton = { TextButton(onClick = { vm.close(); closeDialog = false }) { Text("Encerrar sessão") } },
        dismissButton = { TextButton(onClick = { closeDialog = false }) { Text("Voltar") } })
}
