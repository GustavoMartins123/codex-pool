@file:OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)

package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.platform.testTag
import androidx.lifecycle.compose.LifecycleStartEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.gustavomartins.codexpool.data.*

@Composable
fun SessionScreen(vm: SessionViewModel, onBack: () -> Unit) {
    val state by vm.state.collectAsStateWithLifecycle()
    var input by rememberSaveable { mutableStateOf("") }
    var closeDialog by remember { mutableStateOf(false) }
    var deleteDialog by remember { mutableStateOf(false) }
    var modelDialog by remember { mutableStateOf(false) }
    LaunchedEffect(state.deleted) { if (state.deleted) onBack() }
    LifecycleStartEffect(vm) {
        vm.connect()
        onStopOrDispose { vm.disconnect() }
    }
    Scaffold(topBar = {
        TopAppBar(title = { Text(state.session?.let { "${it.agent.name} · ${it.model}" } ?: "Sessão", maxLines = 1, overflow = TextOverflow.Ellipsis) },
            navigationIcon = { TextButton(onClick = onBack) { Text("Voltar") } },
            actions = {
                if (state.session?.state == SessionState.Closed) {
                    TextButton(onClick = { deleteDialog = true }, enabled = !state.busy) { Text("Apagar", color = MaterialTheme.colorScheme.error) }
                }
                if (state.session != null && state.session?.state !in listOf(SessionState.Closed, SessionState.Closing)) {
                    TextButton(onClick = { closeDialog = true }, enabled = !state.busy && state.connection == StreamConnection.Live) { Text("Encerrar", color = MaterialTheme.colorScheme.error) }
                }
            })
    }, bottomBar = {
        if (state.session?.state != SessionState.Closed) {
            Surface(tonalElevation = 4.dp) {
                Column(Modifier.fillMaxWidth().navigationBarsPadding().imePadding().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    TextButton(onClick = { modelDialog = true; vm.loadModels() }, enabled = !state.busy && state.session != null, modifier = Modifier.testTag("session-model-settings")) {
                        Text(state.turnSettings?.let { "${it.model} · ${effortLabel(it.reasoningEffort)}" } ?: "Modelo e raciocínio",
                            maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                    OutlinedTextField(input, { input = it }, Modifier.fillMaxWidth().heightIn(max = 180.dp), label = { Text("Mensagem para o agente") }, enabled = state.canSend)
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        OutlinedButton(onClick = vm::cancel, enabled = state.canCancel) { Text("Cancelar turno") }
                        Button(onClick = { vm.send(input) { input = "" } }, enabled = state.canSend && input.isNotBlank()) { Text("Enviar") }
                    }
                }
            }
        }
    }) { padding ->
        SessionEventList(state, Modifier.padding(padding).consumeWindowInsets(padding), vm::connect, vm::decide, vm::loadOlder, vm::resume)

    }
    if (modelDialog && state.session != null && state.turnSettings != null) ModalBottomSheet(onDismissRequest = { modelDialog = false }) {
        Column(Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Text("Modelo e raciocínio", style = MaterialTheme.typography.titleLarge)
            Text("A seleção vale a partir da próxima mensagem. O histórico da conversa é preservado.", style = MaterialTheme.typography.bodyMedium)
            if (state.catalogLoading) LinearProgressIndicator(Modifier.fillMaxWidth())
            ErrorMessage(state.catalogError)
            if (state.catalogError != null) TextButton(onClick = vm::loadModels) { Text("Tentar novamente") }
            state.catalog?.let { ModelSettingsFields(state.session!!.agent, it, state.turnSettings!!, !state.busy && !state.catalogLoading && state.catalogError == null, vm::selectSettings) }
            Button(onClick = { modelDialog = false }, modifier = Modifier.fillMaxWidth()) { Text("Concluir") }
        }
    }
    if (closeDialog) AlertDialog(onDismissRequest = { closeDialog = false }, title = { Text("Encerrar sessão?") },
        text = { Text("O runner encerrará o processo. A conversa e os arquivos serão preservados para retomada.") },
        confirmButton = { TextButton(onClick = { vm.close(); closeDialog = false }) { Text("Encerrar sessão") } },
        dismissButton = { TextButton(onClick = { closeDialog = false }) { Text("Voltar") } })
    if (deleteDialog) AlertDialog(onDismissRequest = { deleteDialog = false }, title = { Text("Apagar conversa permanentemente?") },
        text = { Text("O histórico e os dados de retomada serão apagados. O workspace efêmero também será removido; caminhos próprios e repositórios serão preservados. Conecte o runner original para concluir.") },
        confirmButton = { TextButton(onClick = { deleteDialog = false; vm.deleteConversation() }) { Text("Apagar permanentemente", color = MaterialTheme.colorScheme.error) } },
        dismissButton = { TextButton(onClick = { deleteDialog = false }) { Text("Cancelar") } })
}
