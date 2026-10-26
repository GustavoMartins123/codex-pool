package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.unit.dp
import com.gustavomartins.codexpool.data.BootstrapProgress
import com.gustavomartins.codexpool.data.BootstrapStep
import com.gustavomartins.codexpool.data.Decision

@Composable
fun PairingDialog(state: DashboardState, vm: DashboardViewModel, onScan: (() -> Unit)?) {
    var link by rememberSaveable { mutableStateOf("") }
    var code by rememberSaveable(state.pairing?.id) { mutableStateOf("") }
    val focus = LocalFocusManager.current
    val keyboard = LocalSoftwareKeyboardController.current
    val scroll = rememberScrollState()
    LaunchedEffect(state.pairing?.id, state.pairingComplete) {
        if (state.pairing != null || state.pairingComplete) {
            focus.clearFocus()
            keyboard?.hide()
            scroll.scrollTo(0)
        }
    }
    AlertDialog(onDismissRequest = vm::dismissPairing,
        title = { Text(if (state.pairingComplete) "Decisão enviada" else "Conectar máquina") },
        text = {
            Column(Modifier.heightIn(max = 480.dp).verticalScroll(scroll), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                val bootstrap = state.bootstrap
                if (bootstrap != null) {
                    BootstrapContent(bootstrap, vm)
                } else if (state.pairingComplete) {
                    Text("O computador receberá a decisão. A credencial fica somente na máquina.")
                } else if (state.pairing == null) {
                    Text("Abra o runnerd no computador e leia o QR que ele mostrar. Celular e computador precisam se alcançar pela rede local.")
                    Text("Só autorize máquinas que você reconhece. Quem fotografar o QR pode tentar parear a máquina enquanto ele valer.")
                    if (onScan != null) OutlinedButton(onClick = onScan, enabled = !state.loading) { Text("Ler QR Code") }
                    OutlinedTextField(link, { link = it }, label = { Text("Link de pareamento") }, enabled = !state.loading)
                    Button(onClick = { vm.reviewPairing(link.trim()) }, enabled = !state.loading && link.isNotBlank()) { Text("Verificar máquina") }
                } else {
                    val machine = state.pairing
                    Text(machine.name, style = MaterialTheme.typography.titleLarge)
                    Text("${machine.os} · ${machine.arch}")
                    Text(if (machine.runnerId == null) "Esta máquina poderá executar sessões da sua conta." else "A credencial desta máquina será substituída.")
                    Text("Confira a máquina e digite o código exibido ao lado do QR no computador.")
                    OutlinedTextField(code, { code = it }, label = { Text("Código no computador") }, singleLine = true, enabled = !state.loading)
                    Text("Expira em ${machine.expiresAt}", style = MaterialTheme.typography.bodySmall)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = { vm.decidePairing(machine.id, Decision.Deny, "") }, enabled = !state.loading) { Text("Negar") }
                        Button(onClick = { vm.decidePairing(machine.id, Decision.Approve, code.trim()) }, enabled = !state.loading && code.trim().length == 8) { Text("Autorizar máquina") }
                    }
                }
                ErrorMessage(state.error)
                if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
            }
        },
        confirmButton = { TextButton(onClick = vm::dismissPairing, enabled = !state.loading) { Text("Concluir") } })
}

@Composable
private fun BootstrapContent(progress: BootstrapProgress, vm: DashboardViewModel) {
    val ticket = progress.ticket
    when (progress.step) {
        BootstrapStep.Connecting -> Text("Conectando ao computador pela rede local…")
        BootstrapStep.Verifying -> Text("Conferindo o pedido no pool…")
        BootstrapStep.Confirm -> if (ticket != null) {
            Text(ticket.name, style = MaterialTheme.typography.titleLarge)
            Text("${ticket.os} · ${ticket.arch}")
            Text("Esta máquina poderá executar sessões da sua conta em ${ticket.poolUrl.removePrefix("https://")}. " +
                "A credencial fica só no computador e a chave de comandos fica só neste aparelho.")
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedButton(onClick = vm::denyBootstrap) { Text("Negar") }
                Button(onClick = vm::approveBootstrap) { Text("Autorizar máquina") }
            }
        }
        BootstrapStep.Authorizing -> Text("Salvando a chave e autorizando…")
        BootstrapStep.Saving -> Text("Autorizado. Aguardando o computador salvar a credencial…")
        BootstrapStep.WaitingOnline -> Text(progress.message ?: "Pareado. Aguardando o runner ficar online…")
        BootstrapStep.Online -> Text("${ticket?.name ?: "Runner"} está online.")
        BootstrapStep.Cancelled -> Text("Pareamento cancelado.")
        BootstrapStep.Failed -> ErrorMessage(progress.message)
    }
    if (progress.step in setOf(BootstrapStep.Connecting, BootstrapStep.Verifying, BootstrapStep.Authorizing, BootstrapStep.Saving) ||
        progress.step == BootstrapStep.WaitingOnline && progress.message == null) {
        LinearProgressIndicator(Modifier.fillMaxWidth())
    }
}
