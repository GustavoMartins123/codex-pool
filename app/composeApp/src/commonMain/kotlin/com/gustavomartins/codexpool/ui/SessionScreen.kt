@file:OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)

package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
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
    LifecycleStartEffect(vm) {
        vm.connect()
        onStopOrDispose { vm.disconnect() }
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
        SessionEventList(state, Modifier.padding(padding).consumeWindowInsets(padding), vm::connect, vm::decide, vm::loadOlder)

    }
    if (closeDialog) AlertDialog(onDismissRequest = { closeDialog = false }, title = { Text("Encerrar sessão?") },
        text = { Text("O runner encerrará o processo do agente e limpará o workspace efêmero. Workspaces por caminho ou repositório serão preservados.") },
        confirmButton = { TextButton(onClick = { vm.close(); closeDialog = false }) { Text("Encerrar sessão") } },
        dismissButton = { TextButton(onClick = { closeDialog = false }) { Text("Voltar") } })
}
